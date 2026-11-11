package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/garamsh/mvmd/daemon/internal/convertchannel"
	"github.com/garamsh/mvmd/daemon/internal/fencing"
	"github.com/garamsh/mvmd/daemon/internal/image"
	imagememory "github.com/garamsh/mvmd/daemon/internal/image/memory"
	"github.com/garamsh/mvmd/daemon/internal/imagestore"
	"github.com/garamsh/mvmd/daemon/internal/machine"
	machinememory "github.com/garamsh/mvmd/daemon/internal/machine/memory"
	"github.com/garamsh/mvmd/daemon/internal/ociregistry"
	"github.com/garamsh/mvmd/daemon/internal/supervision"
)

// conversionSupervision is the supervision the converter VMs run under
// (machine.Supervision). It is its own instance over its own state directory
// and VMM cgroup, never the one the fencing deadline renews: a conversion's
// supervisor expires at the end of the conversion limit, and a renewal of the
// deadline would extend it. Its Live is what a restart reaps (see reap).
type conversionSupervision struct {
	sup   fencing.Supervisors
	clock fencing.Clock
	limit time.Duration
}

var _ machine.Supervision = conversionSupervision{}

func (c conversionSupervision) RunDir(id machine.MachineID) (string, error) { return c.sup.RunDir(id) }

// Start gives the supervisor the end of the conversion limit, counted from now,
// as its expiry.
func (c conversionSupervision) Start(ctx context.Context, id machine.MachineID, process machine.ProcessDescription, user machine.User) error {
	return c.sup.Start(ctx, id, process, user, c.clock.Now().Add(c.limit))
}

func (c conversionSupervision) Terminate(ctx context.Context, id machine.MachineID) error {
	return c.sup.Terminate(ctx, id)
}

func (c conversionSupervision) RemoveState(ctx context.Context, id machine.MachineID) error {
	return c.sup.RemoveState(ctx, id)
}

func (c conversionSupervision) ListState(ctx context.Context) ([]machine.MachineID, error) {
	return c.sup.ListState(ctx)
}

func (c conversionSupervision) Live(ctx context.Context) ([]machine.MachineID, error) {
	live, err := c.sup.Live(ctx)
	if err != nil {
		return nil, err
	}
	ids := make([]machine.MachineID, len(live))
	for i, s := range live {
		ids[i] = s.Machine
	}
	return ids, nil
}

// images is the image domain as the composition site holds it: the pull use
// case, and the supervision its converter VMs run under.
type images struct {
	service     image.Service
	conversions machine.Supervision
	// driver is the Driver the converter VMs boot on; the memory images leave
	// it to the substitutes.
	driver machine.Driver
}

// reap ends the converter VMs a previous mvmd left running, before anything is
// served. The VMM of a conversion can outlive mvmd, and the user it runs as was
// allocated in the memory of the mvmd that is gone: the allocator of this one
// holds nothing for it, so that user could be handed to a Machine while the VMM
// still runs as it. Ending the VMM frees the user without the allocator having
// to learn it. The conversion's staging area is what the sweep removes next
// (image.md §Pull image).
func (i images) reap(ctx context.Context) error {
	live, err := i.conversions.Live(ctx)
	if err != nil {
		return fmt.Errorf("list the conversions that outlived mvmd: %w", err)
	}
	var errs []error
	for _, id := range live {
		if err := i.conversions.Terminate(ctx, id); err != nil {
			errs = append(errs, fmt.Errorf("end conversion %q: %w", id, err))
		}
	}
	return errors.Join(errs...)
}

// memorySettings are the settings of the pull use case over the in-memory
// substitutes, which read no node configuration.
func memorySettings() image.Settings {
	return image.Settings{
		Platform:     platform(),
		Converter:    "memory",
		Resources:    machine.Resources{VCPUs: 1, MemoryBytes: 1 << 28},
		OutputBytes:  1 << 20,
		NameSetBytes: 1 << 16,
		Limit:        time.Minute,
	}
}

// memoryImages is the pull use case over the in-memory Registry, converter
// and image cache, which the substitutes expose to a test.
func memoryImages(sub *Substitutes, uids *machine.UIDs, pullTimeout time.Duration) images {
	sub.Registry = imagememory.NewRegistry()
	sub.Converter = imagememory.NewConverter()
	sub.Cache = imagememory.NewStore()
	sub.ConversionDriver = machinememory.NewDriver(machine.DriverInfo{Name: "memory", Version: "1"})
	sub.ConversionSupervision = machinememory.NewSupervision()
	return images{
		service:     image.NewService(sub.Registry, sub.Converter, sub.Cache, uids, sub.ConversionDriver, sub.ConversionSupervision, memorySettings(), image.WithPullTimeout(pullTimeout)),
		conversions: sub.ConversionSupervision,
	}
}

// realImages constructs the Registry, the converter channel and the Image
// Store, with the Driver and the supervision of conversions, and returns the
// store the lifecycle locates images in. Each input is checked here, so a node
// whose kernel is not the pinned one, whose store is on a filesystem without
// fs-verity or whose credentials are readable by others does not start.
func realImages(cfg Config, clock fencing.Clock, uids *machine.UIDs, pullTimeout time.Duration) (images, machine.ImageStore, error) {
	settings, err := cfg.Conversion.settings()
	if err != nil {
		return images{}, nil, err
	}
	vmm, _ := cfg.vmm()
	s := cfg.Supervision
	vmmCgroup := s.VMMCgroup
	if vmmCgroup == "" {
		vmmCgroup = supervision.DefaultVMMCgroup
	}
	sup, err := supervision.New(supervision.Config{
		Binary: s.Binary, Dir: filepath.Join(cfg.StateDir, "conversions"), Cgroup: s.Cgroup,
		// Another parent than the Machines', so that a client's Machine named
		// like a conversion cannot share its cgroup.
		VMMCgroup: vmmCgroup + "-conversion",
	})
	if err != nil {
		return images{}, nil, err
	}
	conversions := conversionSupervision{sup: sup, clock: clock, limit: settings.Limit}
	driver, err := cfg.Conversion.files(vmm).driver(cfg.Driver, sup)
	if err != nil {
		return images{}, nil, fmt.Errorf("the converter's Driver: %w", err)
	}
	store, err := imagestore.New(filepath.Join(cfg.StateDir, "images"), wallClock{})
	if err != nil {
		return images{}, nil, err
	}
	opts := ociregistry.Options{}
	if r := cfg.Registry; r != nil {
		creds, err := loadCredentials(r.CredentialsFile)
		if err != nil {
			return images{}, nil, err
		}
		opts.Credentials = creds
	}
	service := image.NewService(ociregistry.New(opts), convertchannel.New(driver), store, uids, driver, conversions, settings, image.WithPullTimeout(pullTimeout))
	return images{service: service, conversions: conversions, driver: driver}, store, nil
}

// loadCredentials reads the credentials file and returns the lookup the
// Registry adapter calls. A file that others can read, or that does not parse,
// is refused. No error names a username or a password.
func loadCredentials(path string) (func(context.Context, string) (ociregistry.Credential, error), error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("registry.credentials_file: %w", err)
	}
	defer f.Close()
	info, err := f.Stat()
	switch {
	case err != nil:
		return nil, fmt.Errorf("registry.credentials_file: %w", err)
	case !info.Mode().IsRegular():
		return nil, fmt.Errorf("registry.credentials_file %q is not a regular file", path)
	case info.Mode().Perm()&0o077 != 0:
		return nil, fmt.Errorf("registry.credentials_file %q is accessible to group or others (mode %04o); it must be 0600 or closer", path, info.Mode().Perm())
	}
	var byRegistry map[string]struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	dec := json.NewDecoder(f)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&byRegistry); err != nil {
		// The decoding error is left out: it can quote a character of the file.
		return nil, fmt.Errorf("registry.credentials_file %q does not hold a JSON object of registries to username and password", path)
	}
	creds := make(map[string]ociregistry.Credential, len(byRegistry))
	for registry, c := range byRegistry {
		if c.Username == "" || c.Password == "" {
			return nil, fmt.Errorf("registry.credentials_file: the entry of %q has no username or no password", registry)
		}
		creds[registry] = ociregistry.Credential{Username: c.Username, Password: c.Password}
	}
	return func(_ context.Context, registry string) (ociregistry.Credential, error) {
		return creds[registry], nil // an unlisted registry is anonymous
	}, nil
}
