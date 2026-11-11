//go:build e2e_real

package e2e_test

import (
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/lib/runtimeapi/v1alpha2"
)

// gateway is the node's IPv4 gateway of node_test.go's network section. mvmd puts it on each tap
// as a host address, so a listener on the host is reachable from a guest at gateway:port and the
// host's packets to a guest leave from it.
const gateway = "10.77.0.1"

// tcpAllow is a rule that allows TCP to or from one host and one port.
func tcpAllow(host string, port int) *v1alpha2.NetworkRule {
	return &v1alpha2.NetworkRule{
		Prefix:   host + "/32",
		Protocol: v1alpha2.Protocol_PROTOCOL_TCP,
		Ports:    []*v1alpha2.PortRange{{First: uint32(port), Last: uint32(port)}},
	}
}

// policyMachine creates a Machine of the image under the policy, starts it, and returns its IPv4
// address. It reads the policy and the address back: a Machine that came up under another policy
// would pass the dials below for the wrong reason.
func (n *node) policyMachine(t *testing.T, ctx context.Context, id, image string, policy *v1alpha2.NetworkPolicy) string {
	t.Helper()
	req := createRequest(id, image)
	req.NetworkPolicy = policy
	n.runMachine(t, ctx, req)
	inspected, err := n.client.InspectMachine(ctx, &v1alpha2.InspectMachineRequest{MachineId: id})
	require.NoError(t, err)
	got := inspected.Machine.NetworkPolicy
	assert.Equal(t, describeRules(policy.GetEgress()), describeRules(got.GetEgress()), "the egress rules %s is in effect under", id)
	assert.Equal(t, describeRules(policy.GetIngress()), describeRules(got.GetIngress()), "the ingress rules %s is in effect under", id)
	require.Len(t, inspected.Machine.Addresses, 1, "the node serves IPv4 only")
	return inspected.Machine.Addresses[0]
}

// describeRules renders rules as comparable strings.
func describeRules(rules []*v1alpha2.NetworkRule) []string {
	out := []string{}
	for _, rule := range rules {
		ports := []string{}
		for _, p := range rule.Ports {
			ports = append(ports, fmt.Sprintf("%d-%d", p.First, p.Last))
		}
		out = append(out, fmt.Sprintf("%s %s %s", rule.Prefix, rule.Protocol, strings.Join(ports, ",")))
	}
	return out
}

// hostListener is a TCP listener the test serves on every address of the host, the gateway among
// them, and the remote addresses that connected to it.
type hostListener struct {
	port int
	mu   sync.Mutex
	from []string
}

func listenOnHost(t *testing.T) *hostListener {
	t.Helper()
	listener, err := net.Listen("tcp4", "0.0.0.0:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })
	l := &hostListener{port: listener.Addr().(*net.TCPAddr).Port}
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			l.mu.Lock()
			l.from = append(l.from, conn.RemoteAddr().(*net.TCPAddr).IP.String())
			l.mu.Unlock()
			conn.Close()
		}
	}()
	return l
}

// connections counts the connections the listener accepted from the address.
func (l *hostListener) connections(from string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	count := 0
	for _, f := range l.from {
		if f == from {
			count++
		}
	}
	return count
}

// guestDial dials the address from inside the Machine and returns how the guest says it ended.
func (n *node) guestDial(t *testing.T, ctx context.Context, machine, id, address string) string {
	t.Helper()
	out, code := n.exec(t, ctx, machine, id, "/bin/entry", "dial", address)
	require.Zero(t, code, "the dial command ran")
	return strings.TrimSpace(out)
}

// hostDial dials the Machine's address from the host, from the gateway address, and returns how
// the host saw it end.
func hostDial(address string) string {
	dialer := net.Dialer{Timeout: 4 * time.Second, LocalAddr: &net.TCPAddr{IP: net.ParseIP(gateway)}}
	conn, err := dialer.Dial("tcp4", address)
	if err == nil {
		conn.Close()
		return "connected"
	}
	if nerr, ok := err.(net.Error); ok && nerr.Timeout() {
		return "timeout"
	}
	return err.Error()
}

// A Machine under an empty policy reaches nothing: its dial to a listener on the host's gateway
// address times out, the kind of failure a dropped packet gives and a refusal does not. A Machine
// under an egress rule reaches the allowed address and port, and a second port of the same
// listener host, which no rule allows, times out the same way. The listeners exist throughout:
// the host reaches each of them, and the host's count says who connected.
func TestE2E_AnEgressRuleAllowsItsAddressAndPortAndAnEmptyPolicyReachesNothing(t *testing.T) {
	n := startNode(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	n.arm(t)
	pulled := n.pullBootable(t, "egress")

	allowed, other := listenOnHost(t), listenOnHost(t)
	closed := n.policyMachine(t, ctx, "closed", pulled.Digest, &v1alpha2.NetworkPolicy{Egress: []*v1alpha2.NetworkRule{&v1alpha2.NetworkRule{Prefix: "0.0.0.0/0", Protocol: v1alpha2.Protocol_PROTOCOL_ANY}}})
	open := n.policyMachine(t, ctx, "open", pulled.Digest, &v1alpha2.NetworkPolicy{
		Egress: []*v1alpha2.NetworkRule{tcpAllow(gateway, allowed.port)},
	})

	// The listeners are there: the host connects to each of them.
	for _, l := range []*hostListener{allowed, other} {
		conn, err := net.DialTimeout("tcp4", fmt.Sprintf("127.0.0.1:%d", l.port), 4*time.Second)
		require.NoError(t, err, "the listener on port %d", l.port)
		conn.Close()
	}

	// The control: the allowed address and port is reached, and the listener saw the Machine.
	assert.Equal(t, "connected", n.guestDial(t, ctx, "open", "allowed", fmt.Sprintf("%s:%d", gateway, allowed.port)))
	require.Eventually(t, func() bool { return allowed.connections(open) == 1 }, 10*time.Second, 100*time.Millisecond,
		"the listener on the allowed port saw a connection from %s", open)

	assert.Equal(t, "timeout", n.guestDial(t, ctx, "closed", "empty-allowed", fmt.Sprintf("%s:%d", gateway, allowed.port)), "an empty policy, the port the rule above allows")
	assert.Equal(t, "timeout", n.guestDial(t, ctx, "closed", "empty-other", fmt.Sprintf("%s:%d", gateway, other.port)), "an empty policy")
	assert.Equal(t, "timeout", n.guestDial(t, ctx, "open", "other", fmt.Sprintf("%s:%d", gateway, other.port)), "a second port of the allowed host, which no rule names")

	assert.Zero(t, allowed.connections(closed), "the Machine with no policy reached the listener")
	assert.Zero(t, other.connections(closed), "the Machine with no policy reached the listener")
	assert.Zero(t, other.connections(open), "the Machine reached a port its rule does not name")
}

// startListening runs a command in the Machine that listens on the ports and returns once the
// guest says each is bound; the command stays running.
func (n *node) startListening(t *testing.T, ctx context.Context, machine string, ports ...int) {
	t.Helper()
	command := []string{"/bin/entry", "listen"}
	var want []string
	for _, port := range ports {
		command = append(command, fmt.Sprint(port))
		want = append(want, fmt.Sprintf("listening %d\n", port))
	}
	id := "listener-" + machine
	started, err := n.client.Exec(ctx, &v1alpha2.ExecRequest{ExecId: id, RequestId: "exec-" + id, MachineId: machine, Command: command})
	require.NoError(t, err)
	require.Equal(t, v1alpha2.ExecStatus_EXEC_STATUS_STARTED, started.Record.Status, started.Record.Reason)

	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	var out strings.Builder
	_, err = v1alpha2.ReadExecOutput(ctx, n.client, machine, id, 0, func(c *v1alpha2.OutputChunk) error {
		out.Write(c.GetData())
		for _, line := range want {
			if !strings.Contains(out.String(), line) {
				return nil
			}
		}
		return errFirstLineIn
	})
	require.ErrorIs(t, err, errFirstLineIn, "the listener's output so far on %s: %q", machine, out.String())
}

// The same for ingress. Both Machines listen on two ports in the guest. The host dials from the
// gateway address: under an empty policy the dial times out, and under an ingress rule for that
// address and one port it connects to that port and times out at the other.
func TestE2E_AnIngressRuleAllowsItsSourceAndPortAndAnEmptyPolicyAdmitsNothing(t *testing.T) {
	n := startNode(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	n.arm(t)
	pulled := n.pullBootable(t, "ingress")

	// Two ports no other process of the guest holds.
	const allowed, other = 8081, 8082
	closed := n.policyMachine(t, ctx, "closed", pulled.Digest, &v1alpha2.NetworkPolicy{})
	open := n.policyMachine(t, ctx, "open", pulled.Digest, &v1alpha2.NetworkPolicy{
		Ingress: []*v1alpha2.NetworkRule{tcpAllow(gateway, allowed)},
	})
	n.startListening(t, ctx, "closed", allowed, other)
	n.startListening(t, ctx, "open", allowed, other)

	// The control: the allowed source and port is reached.
	assert.Equal(t, "connected", hostDial(fmt.Sprintf("%s:%d", open, allowed)))

	assert.Equal(t, "timeout", hostDial(fmt.Sprintf("%s:%d", closed, allowed)), "an empty policy, the port the rule above allows")
	assert.Equal(t, "timeout", hostDial(fmt.Sprintf("%s:%d", closed, other)), "an empty policy")
	assert.Equal(t, "timeout", hostDial(fmt.Sprintf("%s:%d", open, other)), "a second port of the allowed source, which no rule names")
}
