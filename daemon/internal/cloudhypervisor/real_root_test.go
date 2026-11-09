//go:build cloud_hypervisor_real

package cloudhypervisor_test

import (
	"testing"

	"github.com/garamsh/mvmd/daemon/internal/drivertest"
)

// The real tests run in a root of the supervisor's shape (drivertest).
func TestMain(m *testing.M) { drivertest.Main(m) }
