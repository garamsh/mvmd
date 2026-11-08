package v1alpha1

import (
	"fmt"
	"net/netip"
	"regexp"
	"strings"
)

var digestForm = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

func validateDigest(digest string) error {
	if !digestForm.MatchString(digest) {
		return fmt.Errorf("image digest %q is not a sha256 digest", digest)
	}
	return nil
}

// Validate refuses an image reference whose repository carries a tag or an
// "@", or whose digest is not a sha256 digest.
func (r *ImageRef) Validate() error {
	repository := r.GetRepository()
	if repository == "" {
		return fmt.Errorf("image repository is empty")
	}
	if strings.Contains(repository, "@") {
		return fmt.Errorf("image repository %q carries a digest", repository)
	}
	if strings.Contains(repository[strings.LastIndex(repository, "/")+1:], ":") {
		return fmt.Errorf("image repository %q carries a tag", repository)
	}
	return validateDigest(r.GetDigest())
}

func requireIDs(ids map[string]string) error {
	for name, value := range ids {
		if value == "" {
			return fmt.Errorf("%s is empty", name)
		}
	}
	return nil
}

// Validate refuses a creating call without its identifier or request ID, or
// with an image that is not a sha256 digest, or with a network policy that
// is not valid (runtime-api.md).
func (r *CreateMachineRequest) Validate() error {
	if err := requireIDs(map[string]string{"machine_id": r.GetMachineId(), "request_id": r.GetRequestId()}); err != nil {
		return err
	}
	if err := validateDigest(r.GetImageDigest()); err != nil {
		return err
	}
	if r.GetNetworkPolicy() != nil {
		if err := r.GetNetworkPolicy().Validate(); err != nil {
			return err
		}
	}
	return nil
}

// Validate refuses a creating call without its identifier or request ID.
func (r *SnapshotMachineRequest) Validate() error {
	return requireIDs(map[string]string{"snapshot_id": r.GetSnapshotId(), "request_id": r.GetRequestId()})
}

// Validate refuses a creating call without its identifier or request ID, or
// with a network policy that is not valid (runtime-api.md).
func (r *RestoreMachineRequest) Validate() error {
	if err := requireIDs(map[string]string{"machine_id": r.GetMachineId(), "request_id": r.GetRequestId()}); err != nil {
		return err
	}
	if r.GetNetworkPolicy() != nil {
		if err := r.GetNetworkPolicy().Validate(); err != nil {
			return err
		}
	}
	return nil
}

// Validate refuses a creating call without its identifier or request ID.
func (r *ExecRequest) Validate() error {
	return requireIDs(map[string]string{"exec_id": r.GetExecId(), "request_id": r.GetRequestId()})
}

// Validate refuses an image reference that is not a sha256 digest.
func (r *PullImageRequest) Validate() error {
	return r.GetImage().Validate()
}

// Validate refuses a NetworkRule whose fields violate the NetworkPolicy
// contract: an unparseable prefix; an unspecified or out-of-range protocol;
// ICMP with an IPv6 prefix or ICMPv6 with an IPv4 prefix; a port outside
// 1 to 65535; a range with first above last; ports on an ICMP, ICMPv6, or
// any-protocol rule (runtime-api.md).
func (r *NetworkRule) Validate() error {
	p := r.GetProtocol()
	switch p {
	case Protocol_PROTOCOL_TCP, Protocol_PROTOCOL_UDP,
		Protocol_PROTOCOL_ICMP, Protocol_PROTOCOL_ICMPV6, Protocol_PROTOCOL_ANY:
	default:
		return fmt.Errorf("network rule protocol %d is not a valid Protocol value", p)
	}

	prefix, err := netip.ParsePrefix(r.GetPrefix())
	if err != nil {
		return fmt.Errorf("network rule prefix %q: %w", r.GetPrefix(), err)
	}

	switch p {
	case Protocol_PROTOCOL_ICMP:
		if prefix.Addr().Is6() {
			return fmt.Errorf("ICMP rule must not use an IPv6 prefix")
		}
	case Protocol_PROTOCOL_ICMPV6:
		if !prefix.Addr().Is6() {
			return fmt.Errorf("ICMPv6 rule must not use an IPv4 prefix")
		}
	}

	if hasPorts := p == Protocol_PROTOCOL_TCP || p == Protocol_PROTOCOL_UDP; !hasPorts {
		if len(r.GetPorts()) > 0 {
			return fmt.Errorf("ICMP, ICMPv6, and any-protocol rules must not set ports")
		}
		return nil
	}

	for _, pr := range r.GetPorts() {
		if pr.GetFirst() < 1 {
			return fmt.Errorf("port range first must be >= 1, got %d", pr.GetFirst())
		}
		if pr.GetFirst() > 65535 {
			return fmt.Errorf("port range first must be <= 65535, got %d", pr.GetFirst())
		}
		if pr.GetLast() < 1 {
			return fmt.Errorf("port range last must be >= 1, got %d", pr.GetLast())
		}
		if pr.GetLast() > 65535 {
			return fmt.Errorf("port range last must be <= 65535, got %d", pr.GetLast())
		}
		if pr.GetFirst() > pr.GetLast() {
			return fmt.Errorf("port range first (%d) must not be above last (%d)", pr.GetFirst(), pr.GetLast())
		}
	}
	return nil
}

// Validate refuses a NetworkPolicy whose rules are invalid (runtime-api.md).
func (p *NetworkPolicy) Validate() error {
	for i, rule := range p.GetEgress() {
		if err := rule.Validate(); err != nil {
			return fmt.Errorf("egress rule %d: %w", i, err)
		}
	}
	for i, rule := range p.GetIngress() {
		if err := rule.Validate(); err != nil {
			return fmt.Errorf("ingress rule %d: %w", i, err)
		}
	}
	return nil
}
