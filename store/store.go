// Copyright © 2019 KIM KeepInMind GmbH/srl
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU General Public License for more details.
//
// You should have received a copy of the GNU General Public License
// along with this program. If not, see <http://www.gnu.org/licenses/>.

// Package store is the main component that orchestrates sources.
// It provides a `SourceStore` that has to be configured with `New`,
// providing the internal store that the `SourceStore` will use to
// save the sources that it receives.
// The behaviour that `SourceStore` uses to retrieve sources from its
// protected storage can be manipulated adding and removing policies
// to and from it.
package store

import (
	"context"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/booster-proj/booster/core"
	"upspin.io/log"
)

// Store describes an entity that is able to store,
// delete and enumerate sources.
type Store interface {
	Put(...core.Source)
	Del(...core.Source)
	Get(context.Context, ...core.Source) (core.Source, error)

	Len() int
	Do(func(core.Source))
}

// A Policy defines wether a connection to `address` should
// be accepted by source `id`.
type Policy interface {
	ID() string
	Accept(id, address string) bool
}

// A SourceStore is able to keep sources under a set of
// policies, or rules. When it is asked to store a value,
// it performs the policy checks on it, and eventually the
// request is forwarded to the protected store.
type SourceStore struct {
	protected Store

	policies struct {
		sync.Mutex
		val []Policy
	}
	bindHistory struct {
		sync.Mutex
		record bool
		val    map[string]string
	}
}

// DummySource is a representation of a source, suitable
// when other components need information about the sources stored,
// but should not be able to mess with it's actual content.
type DummySource struct {
	ID string `json:"name"`
}

// New creates a New instance of SourceStore, using interally `store`
// as the protected storage.
func New(store Store) *SourceStore {
	return &SourceStore{
		protected: store,
	}
}

// Get is an implementation of booster.Balancer. It provides a source, avoiding
// the ones `blacklisted`. The `blacklisted` list is populated with the sources
// that cannot be accepted due to policy restrictions. The source is then
// retriven from the protected storage.
// If `bindHistory.record == true`, the source identifier returned for this address
// is saved into `bindHistory.val`.
func (ss *SourceStore) Get(ctx context.Context, address string, blacklisted ...core.Source) (core.Source, error) {
	address = TrimPort(address)

	// Combine blacklist received with the one composed by
	// the policies.
	blacklisted = append(blacklisted, ss.MakeBlacklist(address)...)
	log.Debug.Printf("SourceStore: Blacklist for %s: %v", address, blacklisted)

	src, err := ss.protected.Get(ctx, blacklisted...)
	if err != nil {
		return src, err
	}

	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	ss.SaveBindHistory(ctx, src.ID(), address)

	return src, nil
}

// SaveBindHistory saves the association of an address with a source. It
// performs the operation only if it is required, as this is a time
// consuming operation (potentially, due to DNS lookup).
func (ss *SourceStore) SaveBindHistory(ctx context.Context, id, address string) {
	// Save bind history only if required.
	ss.bindHistory.Lock()
	defer ss.bindHistory.Unlock()
	if !ss.bindHistory.record {
		return
	}

	if ss.bindHistory.val == nil {
		ss.bindHistory.val = make(map[string]string)
	}

	// Find all addresses associated with `address`. First check if
	// is is an IP address or an hostname. In the former case
	// find an hostname pointing to this ip.
	host := address
	if ip := net.ParseIP(address); ip != nil {
		// It is an IP
		hosts, err := Resolver.LookupAddr(ctx, address)
		if err != nil {
			log.Error.Printf("SourceStore: SaveBindHistory error: %v", err)
			return
		}
		if len(hosts) == 0 {
			log.Error.Printf("SourceStore: SaveBindHistory error: no hosts associated with %s found", address)
			return
		}
		// we just need one host, no matter which one.
		host = hosts[0]
	}

	addrs, err := Resolver.LookupHost(ctx, host)
	if err != nil {
		log.Error.Printf("SourceStore: SaveBindHistory error: %v", err)
		return
	}

	for _, v := range addrs {
		ss.bindHistory.val[v] = id
	}
}

// ShouldAccept takes `id` and `address`, iterates through the list of policies
// and returns false if the two inputs are not accepted by one of them. The
// offending policy is also returned.
// Returns true if no policy blocks `id` and `address`.
func (ss *SourceStore) ShouldAccept(id, address string) (bool, Policy) {
	ss.policies.Lock()
	defer ss.policies.Unlock()

	if ss.policies.val == nil {
		return true, nil
	}

	// remove port from address if it is present
	address = TrimPort(address)
	for _, p := range ss.policies.val {
		ok := p.Accept(id, address)
		if !ok {
			return ok, p
		}
	}

	return true, nil
}

// MakeBlacklist computes the list of blacklisted sources for `address`, i.e. the
// sources that should not be used to perform a request to `address`, because there
// is one or more policies that do not accept them.
func (ss *SourceStore) MakeBlacklist(address string) []core.Source {
	acc := make([]core.Source, 0, ss.Len())

	// return immediately if there is no policy.
	ss.policies.Lock()
	l := len(ss.policies.val)
	ss.policies.Unlock()

	if l == 0 {
		return acc
	}

	address = TrimPort(address)
	ss.Do(func(src core.Source) {
		if ok, _ := ss.ShouldAccept(src.ID(), address); !ok {
			acc = append(acc, src)
		}
	})

	return acc
}

// Len returns the number of sources available to the store.
func (ss *SourceStore) Len() int {
	return ss.protected.Len()
}

// Do executes `f` on each source of the protected storage.
func (ss *SourceStore) Do(f func(core.Source)) {
	ss.protected.Do(f)
}

// AppendPolicy appends `p` to the end of the list of policies.
func (ss *SourceStore) AppendPolicy(p Policy) error {
	ss.policies.Lock()
	defer ss.policies.Unlock()

	if ss.policies.val == nil {
		ss.policies.val = make([]Policy, 0, 1)
	}

	// Ensure that this is not a duplicate.
	for _, v := range ss.policies.val {
		if v.ID() == p.ID() {
			return fmt.Errorf("source store: a policy with identifier %v is already present", v.ID())
		}
	}

	// Eventually append the new policy.
	ss.policies.val = append(ss.policies.val, p)
	if p.ID() == "stick" {
		ss.RecordBindHistory()
	}

	return nil
}

// DelPolicy removes the policy with identifier `id` from the storage.
func (ss *SourceStore) DelPolicy(id string) error {
	ss.policies.Lock()
	defer ss.policies.Unlock()

	if ss.policies.val == nil {
		return fmt.Errorf("source store: no policies stored")
	}

	// Remove the policy from the storage.
	var j int
	var found bool
	for i, v := range ss.policies.val {
		if v.ID() == id {
			found = true
			j = i
			break
		}
	}
	if !found {
		return fmt.Errorf("source store: no %s policy found", id)
	}
	// avoid any possible memory leak in the underlying array.
	ss.policies.val[j] = nil
	ss.policies.val = append(ss.policies.val[:j], ss.policies.val[j+1:]...)
	if id == "stick" {
		ss.StopRecordingBindHistory()
	}

	return nil
}

// Put adds `sources` to the protected storage.
func (ss *SourceStore) Put(sources ...core.Source) {
	ss.policies.Lock()
	defer ss.policies.Unlock()

	ss.protected.Put(sources...)
}

// Del removes `sources` from the protected storage.
func (ss *SourceStore) Del(sources ...core.Source) {
	ss.policies.Lock()
	defer ss.policies.Unlock()

	ss.protected.Del(sources...)
}

// GetPoliciesSnapshot returns a copy of the current policies
// active in the store.
func (ss *SourceStore) GetPoliciesSnapshot() []Policy {
	ss.policies.Lock()
	defer ss.policies.Unlock()

	acc := make([]Policy, len(ss.policies.val))
	copy(acc, ss.policies.val)
	return acc
}

// GetSourcesSnapshot returns nothing more then a copy of the
// list of sources that the storage is holding.
func (ss *SourceStore) GetSourcesSnapshot() []*DummySource {
	acc := make([]*DummySource, 0, ss.protected.Len())

	ss.protected.Do(func(src core.Source) {
		acc = append(acc, &DummySource{
			ID: src.ID(),
		})
	})

	return acc
}

// RecordBindHistory makes the store keep track of which source is
// assigned to which address.
func (ss *SourceStore) RecordBindHistory() {
	ss.bindHistory.Lock()
	defer ss.bindHistory.Unlock()

	ss.bindHistory.val = make(map[string]string)
	ss.bindHistory.record = true
}

// StopRecordingBindHistory makes the store stop tracking which source is
// assigned to which address. The old history, if any, is discarded.
func (ss *SourceStore) StopRecordingBindHistory() {
	ss.bindHistory.Lock()
	defer ss.bindHistory.Unlock()

	ss.bindHistory.val = nil
	ss.bindHistory.record = false
}

// QueryBindHistory queries the bindHistory for address.
func (ss *SourceStore) QueryBindHistory(address string) (src string, ok bool) {
	ss.bindHistory.Lock()
	defer ss.bindHistory.Unlock()

	if ss.bindHistory.val == nil {
		return
	}

	src, ok = ss.bindHistory.val[address]
	return
}
