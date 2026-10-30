// Copyright 2026 The Casdoor Authors. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package routers

import (
	stdcontext "context"
	"errors"
	"hash/fnv"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/beego/beego/v2/core/logs"
	"github.com/beego/beego/v2/server/web"
	"github.com/beego/beego/v2/server/web/context"
	"github.com/beego/beego/v2/server/web/session"
	_ "github.com/beego/beego/v2/server/web/session/redis"
	_ "github.com/beego/beego/v2/server/web/session/redis_cluster"
	"github.com/casdoor/casdoor/util"
)

const fileSessionGcInterval = time.Hour

// fileSessionGcDeleteLimit caps how many expired session files one GC/cleanup run
// removes, so a backlog cannot turn a single background run into an hours-long walk
const fileSessionGcDeleteLimit = 20000

// fileSessionCountThreshold is the session file count above which the directory is
// considered backed up: synchronous GC walks fast-fail and a background cleaner
// drains the directory in paced batches instead
const fileSessionCountThreshold = 100000

// fileSessionCheckTimeout bounds the startup directory inspection, so a slow or
// huge session directory can never delay service startup: on timeout the check is
// skipped (non-critical) and the conservative async-cleanup path is taken
const fileSessionCheckTimeout = 2 * time.Second

// fileSessionCleanInterval paces the background cleaner between deletion batches
const fileSessionCleanInterval = time.Minute

// walk stop sentinels: filepath.Walk aborts on the first error returned by the walk fn
var (
	errDeleteLimit  = errors.New("session gc delete limit reached")
	errCountLimit   = errors.New("session count limit reached")
	errCheckTimeout = errors.New("session check timeout")
)

// lazySessionProvider wraps a beego provider ("file", "redis", "redis_cluster") so that a new session is only
// written to the store (and its cookie only sent) once it holds data, and a request only writes back the keys
// it changed, so concurrent requests of the same browser don't overwrite each other's session data
type lazySessionProvider struct {
	session.Provider
	gcInterval time.Duration
}

// fileSessionProvider wraps beego's "file" provider so that a new session is only written to disk
// once it holds data, instead of one file for every request that comes without a session cookie
type fileSessionProvider struct {
	session.Provider
	savePath    string
	maxLifetime int64

	stateMu   sync.Mutex
	overLimit bool // last inspection saw more than fileSessionCountThreshold files
	cleaning  bool // a background cleaner goroutine is running
}

type deletedSessionValue struct{}

type lazySessionStore struct {
	provider      session.Provider
	sid           string
	lock          sync.RWMutex
	base          session.Store
	changes       map[interface{}]interface{}
	flushed       bool
	ephemeral     bool
	pendingCookie string
	header        http.Header
}

var sessionWriteLocks [64]sync.Mutex

func init() {
	registerLazySessionProvider("file", fileSessionGcInterval)
	registerLazySessionProvider("redis", 0)
	registerLazySessionProvider("redis_cluster", 0)
}

func registerLazySessionProvider(name string, gcInterval time.Duration) {
	provider, err := session.GetProvider(name)
	if err != nil {
		panic(err)
	}

	// beego's file provider holds its provider-wide lock for the whole session
	// directory walk in SessionGC, which stalls every HTTP request on a backlog.
	// Wrap it with the bounded implementation before the lazy wrapper drives GC.
	if name == "file" {
		provider = &fileSessionProvider{Provider: provider}
	}
	session.Register(GetLazySessionProvider(name), &lazySessionProvider{Provider: provider, gcInterval: gcInterval})
}

// GetLazySessionProvider returns the name of the lazy wrapper registered for a beego session provider
func GetLazySessionProvider(name string) string {
	return "casdoor_" + name
}

func (p *fileSessionProvider) SessionInit(ctx stdcontext.Context, maxLifetime int64, savePath string) error {
	err := p.Provider.SessionInit(ctx, maxLifetime, savePath)
	if err != nil {
		return err
	}

	p.savePath = savePath
	p.maxLifetime = maxLifetime

	// Bounded, timed inspection of the session directory: runs off the startup
	// path, so a huge/slow directory can never block boot (see startupCheck).
	// The periodic GC is driven by lazySessionProvider, whose ticker calls this
	// provider's bounded SessionGC.
	util.SafeGoroutine(p.startupCheck)
	return nil
}

// startupCheck inspects the session directory at boot with a bounded, timed walk.
// beego triggers SessionGC from a startup hook (server/web/hooks.go); with a
// backlog of session files even a lock-free full walk burns minutes of IO right
// at boot. Counting only up to the threshold (or until the check timeout) keeps
// the inspection cheap, and a backed-up directory is handed to the background
// cleaner instead of being drained inline.
func (p *fileSessionProvider) startupCheck() {
	if p.savePath == "" || p.maxLifetime <= 0 {
		return
	}

	start := time.Now()
	count, capped, timedOut := p.countSessions(fileSessionCountThreshold+1, fileSessionCheckTimeout)
	took := time.Since(start)

	switch {
	case timedOut:
		logs.Warn("session dir check under %s timed out after %s at %d files, skipping full count and scheduling async cleanup", p.savePath, took, count)
		p.setOverLimit(true)
		p.startAsyncClean()
	case capped:
		logs.Warn("session dir check found more than %d files under %s in %s, scheduling async cleanup", fileSessionCountThreshold, p.savePath, took)
		p.setOverLimit(true)
		p.startAsyncClean()
	default:
		logs.Info("session dir check: %d session files under %s, took %s", count, p.savePath, took)
		p.setOverLimit(false)
	}
}

// countSessions counts session files (directories excluded) until either limit
// files are seen or timeout elapses, whichever comes first. Both bounds keep the
// walk cheap even on directories with millions of entries.
func (p *fileSessionProvider) countSessions(limit int, timeout time.Duration) (count int, capped bool, timedOut bool) {
	deadline := time.Now().Add(timeout)
	err := filepath.Walk(p.savePath, func(path string, info os.FileInfo, err error) error {
		if err != nil || info == nil || info.IsDir() {
			return nil
		}
		count++
		if count >= limit {
			return errCountLimit
		}
		if time.Now().After(deadline) {
			return errCheckTimeout
		}
		return nil
	})
	switch err {
	case errCountLimit:
		return count, true, false
	case errCheckTimeout:
		return count, false, true
	default:
		// nil (exact count) or an unreadable/missing directory: report what was seen
		return count, false, false
	}
}

// deleteExpired removes session files older than maxLifetime without holding
// beego's provider-wide lock (see SessionGC). It stops after limit deletions and
// reports whether that cap was hit.
func (p *fileSessionProvider) deleteExpired(limit int) (deleted int, capped bool) {
	if p.savePath == "" || p.maxLifetime <= 0 {
		return 0, false
	}

	deadline := time.Now().Unix() - p.maxLifetime
	err := filepath.Walk(p.savePath, func(path string, info os.FileInfo, err error) error {
		if err != nil || info == nil || info.IsDir() {
			return nil
		}
		if info.ModTime().Unix() >= deadline {
			return nil
		}
		if os.Remove(path) != nil {
			return nil
		}
		deleted++
		if deleted >= limit {
			return errDeleteLimit
		}
		return nil
	})
	if err != nil && err != errDeleteLimit {
		logs.Error("session cleanup walk failed under %s, error: %s", p.savePath, err)
		return deleted, false
	}
	return deleted, err == errDeleteLimit
}

// SessionGC replaces beego's FileProvider.SessionGC, which holds the provider-wide
// lock for the whole filepath.Walk over the session directory. Every HTTP request
// takes that same lock in SessionStart (SessionRead/SessionExist), so a large
// session directory stalls all HTTP traffic for the entire walk — observed in
// production as every request parked in FileProvider.SessionExist while the port
// kept listening and no error was logged. beego also kicks this off once at server
// start (server/web/hooks.go), which is why restarts hung immediately as well.
// This implementation never takes the global lock, caps deletions per run, and
// fast-fails while the directory is backed up: draining is then owned by the
// paced background cleaner (startAsyncClean), so neither startup nor request
// handling ever waits on a full-directory walk.
func (p *fileSessionProvider) SessionGC(ctx stdcontext.Context) {
	if p.savePath == "" || p.maxLifetime <= 0 {
		return
	}

	if p.isOverLimit() {
		p.startAsyncClean()
		return
	}

	start := time.Now()
	deleted, capped := p.deleteExpired(fileSessionGcDeleteLimit)
	if deleted > 0 || capped {
		logs.Info("SessionGC removed %d expired session files under %s in %s (capped=%v)", deleted, p.savePath, time.Since(start), capped)
	}
	if capped {
		// deletion cap hit: a backlog is likely, hand draining to the paced cleaner
		p.setOverLimit(true)
		p.startAsyncClean()
	}
}

func (p *fileSessionProvider) isOverLimit() bool {
	p.stateMu.Lock()
	defer p.stateMu.Unlock()
	return p.overLimit
}

func (p *fileSessionProvider) setOverLimit(over bool) {
	p.stateMu.Lock()
	defer p.stateMu.Unlock()
	p.overLimit = over
}

// startAsyncClean drains a backed-up session directory on its own goroutine:
// each round deletes up to fileSessionGcDeleteLimit expired files, then re-counts
// with the bounded check, until the directory is back under the threshold. Rounds
// are paced by fileSessionCleanInterval. If a round finds nothing expired while
// still over the threshold, the cleaner stops: the remaining files are live
// sessions and deleting them would sign users out for no gain.
func (p *fileSessionProvider) startAsyncClean() {
	p.stateMu.Lock()
	if p.cleaning {
		p.stateMu.Unlock()
		return
	}
	p.cleaning = true
	p.stateMu.Unlock()

	util.SafeGoroutine(func() {
		defer func() {
			p.stateMu.Lock()
			p.cleaning = false
			p.stateMu.Unlock()
		}()

		for {
			start := time.Now()
			deleted, _ := p.deleteExpired(fileSessionGcDeleteLimit)
			count, capped, timedOut := p.countSessions(fileSessionCountThreshold+1, fileSessionCheckTimeout)
			logs.Info("session async cleanup: removed %d expired files in %s, %d files seen under %s (capped=%v, timedOut=%v)", deleted, time.Since(start), count, p.savePath, capped, timedOut)

			if !capped && !timedOut {
				p.setOverLimit(false)
				logs.Info("session dir back under %d files, async cleanup finished", fileSessionCountThreshold)
				return
			}
			if deleted == 0 {
				// nothing expired left to drain: stop instead of re-walking forever
				p.setOverLimit(false)
				logs.Warn("session async cleanup stopped with %d+ files under %s: no expired files left to remove", fileSessionCountThreshold, p.savePath)
				return
			}
			time.Sleep(fileSessionCleanInterval)
		}
	})
}

func (p *lazySessionProvider) SessionInit(ctx stdcontext.Context, maxLifetime int64, savePath string) error {
	err := p.Provider.SessionInit(ctx, maxLifetime, savePath)
	if err != nil {
		return err
	}

	if p.gcInterval > 0 {
		util.SafeGoroutine(func() {
			for range time.Tick(p.gcInterval) {
				p.Provider.SessionGC(stdcontext.Background())
			}
		})
	}
	return nil
}

func (p *lazySessionProvider) SessionRead(ctx stdcontext.Context, sid string) (session.Store, error) {
	exists, err := p.SessionExist(ctx, sid)
	if err != nil {
		return nil, err
	}
	if !exists {
		return p.newLazySessionStore(sid, nil), nil
	}

	base, err := p.Provider.SessionRead(ctx, sid)
	if err != nil {
		return nil, err
	}
	return p.newLazySessionStore(sid, base), nil
}

func (p *lazySessionProvider) SessionRegenerate(ctx stdcontext.Context, oldSid, sid string) (session.Store, error) {
	exists, err := p.SessionExist(ctx, oldSid)
	if err != nil {
		return nil, err
	}
	if !exists {
		return p.newLazySessionStore(sid, nil), nil
	}

	base, err := p.Provider.SessionRegenerate(ctx, oldSid, sid)
	if err != nil {
		return nil, err
	}
	return p.newLazySessionStore(sid, base), nil
}

func (p *lazySessionProvider) newLazySessionStore(sid string, base session.Store) *lazySessionStore {
	return &lazySessionStore{provider: p.Provider, sid: sid, base: base, changes: map[interface{}]interface{}{}}
}

func getSessionWriteLock(sid string) *sync.Mutex {
	hash := fnv.New32a()
	_, _ = hash.Write([]byte(sid))
	return &sessionWriteLocks[hash.Sum32()%uint32(len(sessionWriteLocks))]
}

func (s *lazySessionStore) Set(ctx stdcontext.Context, key, value interface{}) error {
	s.lock.Lock()
	defer s.lock.Unlock()
	s.changes[key] = value
	if s.pendingCookie != "" && !s.ephemeral {
		s.header.Add("Set-Cookie", s.pendingCookie)
		s.pendingCookie = ""
	}
	return nil
}

func (s *lazySessionStore) Get(ctx stdcontext.Context, key interface{}) interface{} {
	s.lock.RLock()
	defer s.lock.RUnlock()
	if value, ok := s.changes[key]; ok {
		if _, ok = value.(deletedSessionValue); ok {
			return nil
		}
		return value
	}
	if s.flushed || s.base == nil {
		return nil
	}
	return s.base.Get(ctx, key)
}

func (s *lazySessionStore) Delete(ctx stdcontext.Context, key interface{}) error {
	s.lock.Lock()
	defer s.lock.Unlock()
	s.changes[key] = deletedSessionValue{}
	return nil
}

func (s *lazySessionStore) Flush(ctx stdcontext.Context) error {
	s.lock.Lock()
	defer s.lock.Unlock()
	s.changes = map[interface{}]interface{}{}
	s.flushed = true
	return nil
}

func (s *lazySessionStore) SessionID(ctx stdcontext.Context) string {
	return s.sid
}

func (s *lazySessionStore) hasValue() bool {
	for _, value := range s.changes {
		if _, ok := value.(deletedSessionValue); !ok {
			return true
		}
	}
	return false
}

// SessionRelease writes only what this request changed on top of what the store holds now, instead of
// the copy read at the start of the request, which may be outdated by then
func (s *lazySessionStore) SessionRelease(ctx stdcontext.Context, w http.ResponseWriter) {
	s.lock.Lock()
	defer s.lock.Unlock()
	if s.ephemeral || (!s.flushed && len(s.changes) == 0) {
		return
	}
	if s.base == nil && !s.hasValue() {
		return
	}

	writeLock := getSessionWriteLock(s.sid)
	writeLock.Lock()
	defer writeLock.Unlock()

	store, err := s.provider.SessionRead(ctx, s.sid)
	if err != nil {
		logs.Error("SessionRead failed, error: %s", err)
		return
	}
	if s.flushed {
		_ = store.Flush(ctx)
	}
	for key, value := range s.changes {
		if _, ok := value.(deletedSessionValue); ok {
			_ = store.Delete(ctx, key)
		} else {
			_ = store.Set(ctx, key, value)
		}
	}
	store.SessionRelease(ctx, w)

	s.base = store
	s.changes = map[interface{}]interface{}{}
	s.flushed = false
}

func (s *lazySessionStore) SessionReleaseIfPresent(ctx stdcontext.Context, w http.ResponseWriter) {
	s.lock.RLock()
	present := s.base != nil
	s.lock.RUnlock()
	if present {
		s.SessionRelease(ctx, w)
	}
}

// skipSessionPersistence keeps the session of a request that authenticates itself (API calls with an
// Authorization header, client ID/secret or access key) in memory only, as the caller won't send the cookie back
func skipSessionPersistence(ctx *context.Context) {
	if store, ok := ctx.Input.CruSession.(*lazySessionStore); ok {
		store.lock.Lock()
		store.ephemeral = true
		store.pendingCookie = ""
		store.lock.Unlock()
	}
}

// SessionCookieFilter holds back the cookie beego sends for a new, still empty session until the session gets
// data. Otherwise a request that comes with an outdated session ID (e.g. one replaced at sign-in) answers with
// the cookie of a new empty session, which overwrites the cookie of the signed-in session in the browser
func SessionCookieFilter(ctx *context.Context) {
	store, ok := ctx.Input.CruSession.(*lazySessionStore)
	if !ok {
		return
	}

	store.lock.Lock()
	defer store.lock.Unlock()
	if store.base != nil || len(store.changes) != 0 {
		return
	}

	header := ctx.ResponseWriter.Header()
	prefix := web.BConfig.WebConfig.Session.SessionName + "=" + url.QueryEscape(store.sid) + ";"
	cookies := header.Values("Set-Cookie")
	kept := make([]string, 0, len(cookies))
	for _, cookie := range cookies {
		if store.pendingCookie == "" && strings.HasPrefix(cookie, prefix) {
			store.pendingCookie = cookie
			continue
		}
		kept = append(kept, cookie)
	}
	if store.pendingCookie == "" {
		return
	}

	header.Del("Set-Cookie")
	for _, cookie := range kept {
		header.Add("Set-Cookie", cookie)
	}
	store.header = header
	if store.ephemeral {
		store.pendingCookie = ""
	}
}
