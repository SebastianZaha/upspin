// Copyright 2016 The Upspin Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package perm implements mutation permission checking for servers.
package perm

import (
	"sync"
	"time"

	"upspin.io/access"
	"upspin.io/bind"
	"upspin.io/client/clientutil"
	"upspin.io/errors"
	"upspin.io/log"
	"upspin.io/path"
	"upspin.io/upspin"
	"upspin.io/user"
)

// WritersGroupFile is the name of the Group file that specifies
// writers for a Perm instance.
const WritersGroupFile = "Writers"

// retryTimeout is the interval between re-attempts between failures.
// It is shortened during testing.
var retryTimeout = 30 * time.Second

// onUpdate is a testing stub that is called after each user list update occurs.
var onUpdate = func() {}

// Perm tracks the set of users with write access to a server, as specified by
// the Writers Group file. These might be users who can write blocks to a
// StoreServer or create a root on a DirServer.
type Perm struct {
	cfg upspin.Config

	targetUser upspin.UserName
	targetFile upspin.PathName

	lookupFunc lookupFunc
	watchFunc  watchFunc

	// writers is the set of users allowed to write. If it's nil, all users
	// are allowed. An empty map means no one is allowed.
	writers map[upspin.UserName]bool
	mu      sync.RWMutex // guards writers
}

// lookupFunc looks up name, as defined by upspin.DirServer.
type lookupFunc func(upspin.PathName) (*upspin.DirEntry, error)

// watchFunc watches name, as defined by upspin.DirServer.
type watchFunc func(upspin.PathName, int64, <-chan struct{}) (<-chan upspin.Event, error)

// New creates a new Perm monitoring the target user's Writers Group file,
// resolving the DirServer using the given config. The target user is
// typically the user name of a server, such as a StoreServer or a DirServer.
func New(cfg upspin.Config, ready <-chan struct{}, target upspin.UserName) *Perm {
	const op = "serverutil/perm.New"
	return newPerm(op, cfg, ready, target, nil, nil)
}

// NewWithDir creates a new Perm monitoring the target user's Writers Group
// file which must reside on the given DirServer. The target user is typically
// the user name of a server, such as a StoreServer or a DirServer.
func NewWithDir(cfg upspin.Config, ready <-chan struct{}, target upspin.UserName, dir upspin.DirServer) *Perm {
	const op = "serverutil/perm.NewFromDir"
	return newPerm(op, cfg, ready, target, dir.Lookup, dir.Watch)
}

// newPerm creates a new Perm monitoring the target user's Writers Group file,
// using the provided LookupFunc for lookups and the WatchFunc function to
// watch changes on the writers file. If lookup or watch are nil the DirServer
// is resolved using bind and the given config. The target user is typically
// the user name of a server, such as a StoreServer or a DirServer.
func newPerm(op string, cfg upspin.Config, ready <-chan struct{}, target upspin.UserName, lookup lookupFunc, watch watchFunc) *Perm {
	p := &Perm{
		cfg:        cfg,
		targetUser: target,
		targetFile: upspin.PathName(target) + "/Group/" + WritersGroupFile,
		lookupFunc: lookup,
		watchFunc:  watch,
		writers:    nil, // Start open.
	}

	go func() {
		<-ready
		err := p.Update()
		if err != nil {
			log.Error.Printf("%s: %v", op, err)
		}
		go p.updateLoop(op)
	}()

	return p
}

// updateLoop continuously watches for updates on WritersGroupFile.
// It must be run in a goroutine.
func (p *Perm) updateLoop(op string) {
	var (
		events      <-chan upspin.Event
		accessOrder int64
		done        = func() {}
	)
	for {
		var err error
		if events == nil {
			// Channel is not yet open. Open now.
			doneCh := make(chan struct{})
			done = func() {
				if doneCh != nil {
					close(doneCh)
					doneCh = nil
				}
			}
			// TODO(edpin,adg): start watching at most recently seen order.
			events, err = p.watch(upspin.PathName(p.targetUser)+"/", -1, doneCh)
			if err != nil {
				log.Error.Printf("%s: watch: %s", op, err)
				time.Sleep(retryTimeout)
				continue
			}
		}
		e, ok := <-events
		if !ok {
			log.Debug.Printf("%s: watch channel closed. Re-opening...", op)
			events = nil
			time.Sleep(retryTimeout)
			continue
		}
		if e.Error != nil {
			log.Error.Printf("%s: watch event error: %s", op, e.Error)
			done()
			continue
		}
		// An Access file could have granted or revoked our permission
		// to watch the Writers file. Therefore, we must start the Watch
		// again, after the Access event.
		if isRelevantAccess(e.Entry.Name) && e.Order > accessOrder {
			accessOrder = e.Order
			done()
			continue
		}
		// Process event.
		if e.Entry.Name != p.targetFile {
			continue
		}
		if e.Delete {
			p.deleteUsers()
			continue
		}
		err = p.updateUsers(e.Entry)
		if err != nil {
			log.Error.Printf("%s: updateUsers: %s", op, err)
		}
	}
}

// isRelevantAccess access reports whether name is an Access file in a Group
// directory or at the root.
func isRelevantAccess(name upspin.PathName) bool {
	p, err := path.Parse(name)
	if err != nil {
		log.Error.Printf("serverutil/perm.isRelevantAccess: unexpected error: %s", err)
		return false
	}
	file := p.FilePath()
	return file == "Access" || file == "Group/Access"
}

// Update retrieves and parses the Group file that rules over the set of allowed
// writers. This is mostly only exported for testing, but servers may use it to
// force immediate updates.
func (p *Perm) Update() error {
	entry, err := p.lookup(p.targetFile)
	if err != nil {
		// If the group file does not exist, reset writers map.
		if errors.Match(errors.E(errors.NotExist), err) {
			p.deleteUsers() // Calls onUpdate.
			return nil
		}
		onUpdate() // Even if we failed, unblock tests.
		return err
	}
	return p.updateUsers(entry) // Calls onUpdate.
}

// updateUsers reads the writers Group file entry and updates the user set.
func (p *Perm) updateUsers(entry *upspin.DirEntry) error {
	users, err := p.allowedWriters(entry)
	if err != nil {
		onUpdate() // Even if we failed, unblock tests.
		return err
	}
	log.Printf("serverutil/perm: Setting writers to: %v", users)
	p.set(users)
	onUpdate()
	return nil
}

// allowedWriters reads the contents of the entry, interprets it exactly as
// an access Group file, expanding recursively if needed, and returns the slice
// of users allowed to write to the store.
func (p *Perm) allowedWriters(entry *upspin.DirEntry) ([]upspin.UserName, error) {
	// Pretend this is an Access file, so we can easily use it to retrieve a
	// slice of  all authorized users.
	fakeAccess := "w,d:" + entry.Name
	access.RemoveGroup(entry.Name)
	acc, err := access.Parse(upspin.PathName(p.targetUser+"/"), []byte(fakeAccess))
	if err != nil {
		return nil, err
	}

	return acc.Users(access.Write, p.load)
}

// load loads the contents of a name.
func (p *Perm) load(name upspin.PathName) ([]byte, error) {
	entry, err := p.lookup(name)
	if err != nil {
		return nil, err
	}
	return clientutil.ReadAll(p.cfg, entry)
}

// IsWriter reports whether the user has write privileges on this Perm.
func (p *Perm) IsWriter(u upspin.UserName) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	// Everyone is allowed if there is no Writers Group file.
	if p.writers == nil {
		return true
	}
	// If the special user "all@upspin.io" is present, allow all.
	if p.writers[access.AllUsers] {
		return true
	}
	// Is this exact user allowed?
	if p.writers[u] {
		return true
	}
	// Maybe the domain is wildcarded. Check this case last as it's the most
	// expensive.
	_, _, domain, err := user.Parse(u)
	if err != nil {
		// Should never happen at this point.
		log.Error.Printf("serverutil/perm: unexpected error: %s", err)
		return false
	}
	return p.writers[upspin.UserName("*@"+domain)]
}

func (p *Perm) set(users []upspin.UserName) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.writers = make(map[upspin.UserName]bool)
	for _, u := range users {
		p.writers[u] = true
	}
}

func (p *Perm) deleteUsers() {
	p.mu.Lock()
	p.writers = nil
	p.mu.Unlock()
	onUpdate()
}

func (p *Perm) lookup(name upspin.PathName) (*upspin.DirEntry, error) {
	if f := p.lookupFunc; f != nil {
		return f(name)
	}
	parsed, err := path.Parse(name)
	if err != nil {
		return nil, err
	}
	dir, err := bind.DirServerFor(p.cfg, parsed.User())
	if err != nil {
		return nil, err
	}
	return dir.Lookup(name)
}

func (p *Perm) watch(name upspin.PathName, order int64, done <-chan struct{}) (<-chan upspin.Event, error) {
	if f := p.watchFunc; f != nil {
		return f(name, order, done)
	}
	parsed, err := path.Parse(name)
	if err != nil {
		return nil, err
	}
	dir, err := bind.DirServerFor(p.cfg, parsed.User())
	if err != nil {
		return nil, err
	}
	return dir.Watch(name, order, done)
}
