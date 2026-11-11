package boltstore_test

import (
	"context"
	"net/netip"
	"reflect"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/machine"
)

// notStored names each field of a lifecycle type that the store deliberately
// does not keep, as "Type.Field", with the reason. A field of machine.Record or
// machine.ExecEntry, or of any type they hold, that is not listed here must
// come back from a Save and a Load, so a field added to the lifecycle later is
// caught by the test below without anyone editing it.
var notStored = map[string]string{
	"Machine.Driver":       "Inspect fills it from the Driver in service; a record never held anything but its zero value",
	"Record.Unreadable":    "Load sets it on a record it could not decode; it is not data to keep",
	"ExecEntry.Unreadable": "Load sets it on an entry it could not decode; it is not data to keep",
}

// filler gives every field of a value a distinct non-zero value.
type filler struct {
	t    *testing.T
	next int
	// skipped counts the notStored fields met, so a stale entry is found.
	skipped map[string]int
}

func (f *filler) number() int {
	f.next++
	return 100 + f.next
}

func (f *filler) fill(v reflect.Value) {
	typ := v.Type()
	switch typ {
	case reflect.TypeOf(netip.Addr{}):
		v.Set(reflect.ValueOf(netip.AddrFrom4([4]byte{10, 0, byte(f.number() / 256), byte(f.number())})))
		return
	case reflect.TypeOf(netip.Prefix{}):
		v.Set(reflect.ValueOf(netip.PrefixFrom(netip.AddrFrom4([4]byte{10, byte(f.number()), 0, 0}), 16)))
		return
	case reflect.TypeOf(time.Time{}):
		// A time is an instant in UTC, as the store reads one back.
		v.Set(reflect.ValueOf(time.Date(2026, 11, 11, 12, 34, 56, f.number(), time.UTC)))
		return
	case reflect.TypeOf(machine.State(0)), reflect.TypeOf(machine.Protocol(0)), reflect.TypeOf(machine.ExecStatus(0)):
		// An enum holds one of its own values; 2 is one of each.
		v.SetInt(2)
		return
	case reflect.TypeOf(&machine.Error{}):
		v.Set(reflect.ValueOf(machine.NewError(machine.ErrConflict, "reason %d", f.number())))
		return
	}
	switch typ.Kind() {
	case reflect.String:
		v.SetString("s" + typ.Name() + string(rune('a'+f.number()%26)) + string(rune('a'+f.next%26)))
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v.SetInt(int64(f.number()))
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		v.SetUint(uint64(f.number()))
	case reflect.Pointer:
		v.Set(reflect.New(typ.Elem()))
		f.fill(v.Elem())
	case reflect.Slice:
		v.Set(reflect.MakeSlice(typ, 1, 1))
		f.fill(v.Index(0))
	case reflect.Map:
		v.Set(reflect.MakeMap(typ))
		key, val := reflect.New(typ.Key()).Elem(), reflect.New(typ.Elem()).Elem()
		f.fill(key)
		f.fill(val)
		v.SetMapIndex(key, val)
	case reflect.Struct:
		for i := 0; i < typ.NumField(); i++ {
			field := typ.Field(i)
			if !field.IsExported() {
				f.t.Fatalf("%s.%s is unexported, and the test cannot fill it", typ, field.Name)
			}
			name := typ.Name() + "." + field.Name
			if _, skip := notStored[name]; skip {
				f.skipped[name]++
				continue
			}
			f.fill(v.Field(i))
		}
	default:
		f.t.Fatalf("the test cannot fill a %s (%s)", typ, typ.Kind())
	}
}

func filled[T any](t *testing.T) (T, *filler) {
	t.Helper()
	f := &filler{t: t, skipped: map[string]int{}}
	var v T
	f.fill(reflect.ValueOf(&v).Elem())
	return v, f
}

// Every field a lifecycle type holds comes back from the store, whichever way it
// was saved: a field the conversion drops, or one added to the lifecycle that
// the store does not yet know, fails here.
func TestStore_EveryFieldOfARecordAndAnExecEntryIsStored(t *testing.T) {
	ctx := context.Background()

	record, f := filled[machine.Record](t)
	// The filler is not trusted to have reached everything: it filled the
	// record, a Machine, an exec entry and each type under them.
	require.NotNil(t, record.Machine)
	require.NotEmpty(t, record.Execs)
	require.NotNil(t, record.Request.Failure)
	for name := range notStored {
		assert.Positive(t, f.skipped[name], "%s is listed as not stored, but no such field was met", name)
	}
	// Request.Failure's kind is stored by name, so it is one the store knows.
	record.Machine.State, record.Request.Snapshot = machine.StatePaused, "snap"

	t.Run("a record with its entries", func(t *testing.T) {
		db := open(t, t.TempDir())
		defer db.Close()
		require.NoError(t, db.Machines().Save(ctx, record))
		got, err := db.Machines().Load(ctx)
		require.NoError(t, err)
		require.Len(t, got, 1)
		assert.Equal(t, record, got[0])
	})

	t.Run("an entry saved on its own", func(t *testing.T) {
		db := open(t, t.TempDir())
		defer db.Close()
		bare := record
		bare.Execs = nil
		require.NoError(t, db.Machines().Save(ctx, bare))
		entry := record.Execs[0]
		entry.Machine = record.ID // SaveExec needs the record of its Machine
		require.NoError(t, db.Machines().SaveExec(ctx, entry))
		got, err := db.Machines().Load(ctx)
		require.NoError(t, err)
		require.Len(t, got, 1)
		assert.Equal(t, []machine.ExecEntry{entry}, got[0].Execs)
	})

	t.Run("control: the filler leaves nothing at its zero value", func(t *testing.T) {
		assertNoZero(t, reflect.ValueOf(record), "Record")
	})
}

// assertNoZero requires every field of v to be non-zero, but those notStored.
func assertNoZero(t *testing.T, v reflect.Value, path string) {
	t.Helper()
	typ := v.Type()
	switch typ.Kind() {
	case reflect.Struct:
		if typ == reflect.TypeOf(netip.Addr{}) || typ == reflect.TypeOf(netip.Prefix{}) || typ == reflect.TypeOf(time.Time{}) {
			assert.False(t, v.IsZero(), path)
			return
		}
		for i := 0; i < typ.NumField(); i++ {
			if _, skip := notStored[typ.Name()+"."+typ.Field(i).Name]; skip {
				continue
			}
			assertNoZero(t, v.Field(i), path+"."+typ.Field(i).Name)
		}
	case reflect.Pointer:
		if assert.False(t, v.IsNil(), path) {
			assertNoZero(t, v.Elem(), path)
		}
	case reflect.Slice:
		if assert.NotZero(t, v.Len(), path) {
			assertNoZero(t, v.Index(0), path+"[0]")
		}
	case reflect.Map:
		assert.NotZero(t, v.Len(), path)
	default:
		assert.False(t, v.IsZero(), path)
	}
}
