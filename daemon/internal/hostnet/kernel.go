package hostnet

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/google/nftables"
	"github.com/vishvananda/netlink"

	"github.com/garamsh/mvmd/daemon/internal/machine"
)

// kernelView is which of the adapter's tables and chains the kernel holds.
type kernelView struct {
	inetTable bool
	// chains names the chains of the two tables, by table.
	netdev, inet []string
}

func view(c *nftables.Conn) (kernelView, error) {
	var v kernelView
	tabs, err := c.ListTables()
	if err != nil {
		return v, fmt.Errorf("list tables: %w", err)
	}
	for _, t := range tabs {
		v.inetTable = v.inetTable || (t.Name == tableName && t.Family == nftables.TableFamilyINet)
	}
	chains, err := c.ListChains()
	if err != nil {
		return v, fmt.Errorf("list chains: %w", err)
	}
	for _, ch := range chains {
		switch {
		case ch.Table.Name != tableName:
		case ch.Table.Family == nftables.TableFamilyNetdev:
			v.netdev = append(v.netdev, ch.Name)
		case ch.Table.Family == nftables.TableFamilyINet:
			v.inet = append(v.inet, ch.Name)
		}
	}
	return v, nil
}

// apply makes the Machine's tap and rules, in the order of ADR 0012: the tap
// with its link down, the Machine's rules in one transaction, then the link
// up. A failure at any step removes what the steps before it made. Callers
// hold n.mu.
func (n *Network) apply(_ context.Context, m machineNet) error {
	t := newTables()
	plan, err := t.plan(m)
	if err != nil {
		return err
	}
	c, err := nftables.New()
	if err != nil {
		return fmt.Errorf("open nftables: %w", err)
	}
	have, err := view(c)
	if err != nil {
		return err
	}
	link, err := linkOf(m.tap)
	if err != nil {
		return err
	}
	switch committed := slices.Contains(have.netdev, m.tap) && slices.Contains(have.inet, egressChain(m.tap)); {
	case link != nil && committed:
		return nil
	case link != nil || committed:
		// A call that did not finish left part of it: start again.
		if err := n.withdraw(m.tap); err != nil {
			return fmt.Errorf("remove what an earlier call left: %w", err)
		}
		have, err = view(c)
		if err != nil {
			return err
		}
	}

	if err := n.build(c, t, plan, have, m); err != nil {
		return errors.Join(err, n.withdraw(m.tap))
	}
	return nil
}

func (n *Network) build(c *nftables.Conn, t tables, plan *machinePlan, have kernelView, m machineNet) error {
	link, err := createTap(m)
	if err != nil {
		return err
	}
	if have.inetTable {
		c.AddTable(t.netdev)
	} else {
		n.addBase(c, t)
	}
	plan.add(c)
	if err := c.Flush(); err != nil {
		return fmt.Errorf("commit the rules of %s: %w", m.tap, err)
	}
	if err := netlink.LinkSetUp(link); err != nil {
		return fmt.Errorf("set %s up: %w", m.tap, err)
	}
	return route(link, m)
}

// withdraw takes the tap down, removes the Machine's chains and the jumps to
// them in one transaction, and deletes the tap. What is absent is skipped,
// and a failure of one step does not stop the others. Callers hold n.mu.
func (n *Network) withdraw(tap string) error {
	var errs []error
	link, err := linkOf(tap)
	if err != nil {
		errs = append(errs, err)
	}
	if link != nil && link.Type() != "tuntap" {
		// Not ours to delete, whatever it is called.
		errs = append(errs, fmt.Errorf("the device %s is a %s, not a tap", tap, link.Type()))
		link = nil
	}
	if link != nil {
		if err := netlink.LinkSetDown(link); err != nil {
			errs = append(errs, fmt.Errorf("set %s down: %w", tap, err))
		}
	}
	if err := removeRules(tap); err != nil {
		errs = append(errs, err)
	}
	if link != nil {
		if err := netlink.LinkDel(link); err != nil {
			errs = append(errs, fmt.Errorf("delete %s: %w", tap, err))
		}
	}
	return errors.Join(errs...)
}

// removeRules deletes, in one transaction, the base-chain rules tagged with the
// tap and the Machine's chains.
func removeRules(tap string) error {
	c, err := nftables.New()
	if err != nil {
		return fmt.Errorf("open nftables: %w", err)
	}
	have, err := view(c)
	if err != nil {
		return err
	}
	t := newTables()
	for _, name := range []string{chainForward, chainInput, chainOutput, chainPostrouting} {
		if !slices.Contains(have.inet, name) {
			continue
		}
		base := t.inetChain(name)
		rules, err := c.GetRules(t.inet, base)
		if err != nil {
			return fmt.Errorf("list the rules of %s: %w", name, err)
		}
		for _, r := range rules {
			if string(r.UserData) != tap {
				continue
			}
			r.Table, r.Chain = t.inet, base
			if err := c.DelRule(r); err != nil {
				return fmt.Errorf("delete a rule of %s: %w", name, err)
			}
		}
	}
	drop := func(table *nftables.Table, held []string, name string) {
		if slices.Contains(held, name) {
			ch := &nftables.Chain{Table: table, Name: name}
			c.FlushChain(ch)
			c.DelChain(ch)
		}
	}
	drop(t.inet, have.inet, egressChain(tap))
	drop(t.inet, have.inet, ingressChain(tap))
	drop(t.netdev, have.netdev, tap)
	if err := c.Flush(); err != nil {
		return fmt.Errorf("remove the rules of %s: %w", tap, err)
	}
	return nil
}

// Reconcile implements machine.Network: it takes the identities of every
// Machine the inside holds, replaces mvmd's tables in one transaction with the
// rules their records describe, and deletes every tap that belongs to no
// Machine that may still have a VMM.
//
// A Machine that is Running or Paused is kept, and so is a Faulted one whose tap
// exists, since its VMM may still run; the tap of any other is stale. Any other
// state has no VMM: the lifecycle terminated the supervisors no record claimed
// before it called this, so a Created record's tap and rules go too. A Running
// or Paused Machine whose tap is gone is returned as lost, since no rules can
// protect a device that does not exist.
func (n *Network) Reconcile(_ context.Context, machines []machine.Machine) ([]machine.MachineID, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	var lost []machine.MachineID
	err := n.reconcile(machines, &lost)
	return lost, err
}

func (n *Network) reconcile(machines []machine.Machine, lost *[]machine.MachineID) error {

	held := make(map[machine.MachineID]machine.NetworkIdentity)
	for _, m := range machines {
		if m.Network.MAC != "" {
			held[m.ID] = m.Network
		}
	}
	taps, err := ours()
	if err != nil {
		return err
	}

	var errs []error
	var plans []*machinePlan
	kept := make(map[string]bool)
	var keptNets []machineNet
	t := newTables()
	for _, m := range machines {
		if _, ok := held[m.ID]; !ok {
			continue
		}
		tap := tapName(m.ID)
		_, exists := taps[tap]
		running := m.State == machine.StateRunning || m.State == machine.StatePaused
		switch {
		case !running && !(m.State == machine.StateFaulted && exists):
			continue
		case !exists:
			*lost = append(*lost, m.ID)
			continue
		}
		mn, err := n.machineNet(m.ID, m.Network, m.User, m.Policy)
		if err != nil {
			return err
		}
		plan, err := t.plan(mn)
		if err != nil {
			return fmt.Errorf("Machine %q: %w", m.ID, err)
		}
		plans = append(plans, plan)
		kept[tap] = true
		keptNets = append(keptNets, mn)
	}

	c, err := nftables.New()
	if err != nil {
		return fmt.Errorf("open nftables: %w", err)
	}
	// Adding before deleting makes the replacement work whether or not the
	// tables exist, and all of it is one transaction.
	c.AddTable(t.netdev)
	c.AddTable(t.inet)
	c.DelTable(t.netdev)
	c.DelTable(t.inet)
	n.addBase(c, t)
	for _, p := range plans {
		p.add(c)
	}
	if err := c.Flush(); err != nil {
		return errors.Join(append(errs, fmt.Errorf("replace the tables: %w", err))...)
	}
	n.held = held

	// The routes belong to the tap and outlive mvmd with it; making them again
	// from the records costs nothing and covers one that was removed.
	for _, mn := range keptNets {
		if err := routeRouted(taps[mn.tap], mn); err != nil {
			errs = append(errs, err)
		}
	}
	for name, link := range taps {
		if !kept[name] {
			if err := deleteTap(link); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}
