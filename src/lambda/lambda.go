package lambda

import (
	"bufio"
	"container/list"
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/open-lambda/open-lambda/ol/common"
	"github.com/open-lambda/open-lambda/ol/sandbox"
)

// provides thread-safe getting of lambda functions and collects all
// lambda subsystems (resource pullers and sandbox pools) in one place
type LambdaMgr struct {
	// subsystems (these are thread safe)
	sbPool sandbox.SandboxPool
	*DepTracer
	*PackagePuller // depends on sbPool and DepTracer
	*ImportCache   // depends PackagePuller
	*HandlerPuller // depends on sbPool and ImportCache[optional]

	// storage dirs that we manage
	codeDirs    *common.DirMaker
	scratchDirs *common.DirMaker

	// thread-safe map from a lambda's name to its LambdaFunc
	mapMutex sync.Mutex
	lfuncMap map[string]*LambdaFunc
}

// Represents a single lambda function (the code)
type LambdaFunc struct {
	lmgr *LambdaMgr
	name string

	// lambda code
	lastPull *time.Time
	codeDir  string
	meta     *sandbox.SandboxMeta

	// lambda execution
	funcChan  chan *Invocation // server to func
	instChan  chan *Invocation // func to instances
	doneChan  chan *Invocation // instances to func
	instances *list.List

	// send chan to the kill chan to destroy the instance, then
	// wait for msg on sent chan to block until it is done
	killChan chan chan bool
}

// This is essentially a virtual sandbox.  It is backed by a real
// Sandbox (when it is allowed to allocate one).  It pauses/unpauses
// based on usage, and starts fresh instances when they die.
type LambdaInstance struct {
	lfunc *LambdaFunc

	// snapshot of LambdaFunc, at the time the LambdaInstance is created
	codeDir string
	meta    *sandbox.SandboxMeta

	// send chan to the kill chan to destroy the instance, then
	// wait for msg on sent chan to block until it is done
	killChan chan chan bool
}

// represents an HTTP request to be handled by a lambda instance
type Invocation struct {
	w http.ResponseWriter
	r *http.Request

	// signal to client that response has been written to w
	done chan bool

	// how many milliseconds did ServeHTTP take?  (doesn't count
	// queue time or Sandbox init)
	execMs int
}

// Timeout broker manages automatic timeout for lambda
type TimeoutBroker struct {
	// Suicide timer- i.e. when this timer expires, it will cause the Lambda Instance
	// to try to self destruct
	suicideTimer *time.Timer

	// Corresponding instance (to destroy)
	linst *LambdaInstance

	// Cancel function
	cancel func()

	// True if timeout occurred, default set to false,
	// These mostly act as CVs for synchronization
	timedout     bool
	timerinvalid bool

	// Destruction synchronizer, around timedout
	// A "just in case" for a close timer call
	destlock sync.Mutex
}

func NewLambdaMgr() (res *LambdaMgr, err error) {
	mgr := &LambdaMgr{
		lfuncMap: make(map[string]*LambdaFunc),
	}
	defer func() {
		if err != nil {
			log.Printf("Cleanup Lambda Manager due to error: %v", err)
			mgr.Cleanup()
		}
	}()

	mgr.codeDirs, err = common.NewDirMaker("code", common.Conf.Storage.Code.Mode())
	if err != nil {
		return nil, err
	}
	mgr.scratchDirs, err = common.NewDirMaker("scratch", common.Conf.Storage.Scratch.Mode())
	if err != nil {
		return nil, err
	}

	log.Printf("Create SandboxPool")
	mgr.sbPool, err = sandbox.SandboxPoolFromConfig("sandboxes", common.Conf.Mem_pool_mb)
	if err != nil {
		return nil, err
	}

	log.Printf("Create DepTracer")
	mgr.DepTracer, err = NewDepTracer(filepath.Join(common.Conf.Worker_dir, "dep-trace.json"))
	if err != nil {
		return nil, err
	}

	log.Printf("Create PackagePuller")
	mgr.PackagePuller, err = NewPackagePuller(mgr.sbPool, mgr.DepTracer)
	if err != nil {
		return nil, err
	}

	if common.Conf.Features.Import_cache {
		log.Printf("Create ImportCache")
		mgr.ImportCache, err = NewImportCache(mgr.codeDirs, mgr.scratchDirs, mgr.sbPool, mgr.PackagePuller)
		if err != nil {
			return nil, err
		}
	}

	log.Printf("Create HandlerPuller")
	mgr.HandlerPuller, err = NewHandlerPuller(mgr.codeDirs)
	if err != nil {
		return nil, err
	}

	return mgr, nil
}

// Returns an existing instance (if there is one), or creates a new one
func (mgr *LambdaMgr) Get(name string) (f *LambdaFunc) {
	mgr.mapMutex.Lock()
	defer mgr.mapMutex.Unlock()

	f = mgr.lfuncMap[name]

	if f == nil {
		f = &LambdaFunc{
			lmgr:      mgr,
			name:      name,
			funcChan:  make(chan *Invocation, 32),
			instChan:  make(chan *Invocation, 32),
			doneChan:  make(chan *Invocation, 32),
			instances: list.New(),
			killChan:  make(chan chan bool, 1),
		}

		go f.Task()
		mgr.lfuncMap[name] = f
	}

	return f
}

func (mgr *LambdaMgr) Debug() string {
	return mgr.sbPool.DebugString() + "\n"
}

func (mgr *LambdaMgr) Cleanup() {
	mgr.mapMutex.Lock() // don't unlock, because this shouldn't be used anymore

	// HandlerPuller+PackagePuller requires no cleanup

	// 1. cleanup handler Sandboxes
	// 2. cleanup Zygote Sandboxes (after the handlers, which depend on the Zygotes)
	// 3. cleanup SandboxPool underlying both of above
	for _, f := range mgr.lfuncMap {
		log.Printf("Kill function: %s", f.name)
		f.Kill()
	}

	if mgr.ImportCache != nil {
		mgr.ImportCache.Cleanup()
	}

	if mgr.sbPool != nil {
		mgr.sbPool.Cleanup() // assumes all Sandboxes are gone
	}

	// cleanup DepTracer
	if mgr.DepTracer != nil {
		mgr.DepTracer.Cleanup()
	}

	if mgr.codeDirs != nil {
		mgr.codeDirs.Cleanup()
	}

	if mgr.scratchDirs != nil {
		mgr.scratchDirs.Cleanup()
	}
}

func (f *LambdaFunc) Invoke(w http.ResponseWriter, r *http.Request) {
	t := common.T0("LambdaFunc.Invoke")
	defer t.T1()

	done := make(chan bool)
	req := &Invocation{w: w, r: r, done: done}

	// send invocation to lambda func task, if room in queue
	select {
	case f.funcChan <- req:
		// block until it's done
		<-done
	default:
		// queue cannot accept more, so reply with backoff
		req.w.WriteHeader(http.StatusTooManyRequests)
		req.w.Write([]byte("lambda function queue is full"))
	}
}

// add function name to each log message so we know which logs
// correspond to which LambdaFuncs
func (f *LambdaFunc) printf(format string, args ...interface{}) {
	msg := fmt.Sprintf(format, args...)
	log.Printf("%s [FUNC %s]", strings.TrimRight(msg, "\n"), f.name)
}

// the function code may contain comments such as the following:
//
// # ol-install: parso,jedi,idna,chardet,certifi,requests
// # ol-import: parso,jedi,idna,chardet,certifi,requests,urllib3
// # ol-timeout: 30
//
// The first list should be installed with pip install.  The second is
// a hint about what may be imported (useful for import cache).
//
// ol-timeout is used to specify a lambda timeout in milliseconds. If the timeout
// specified is longer than the environment's global timeout, then the gloval
// timeout will be used
//
// We support exact pkg versions (e.g., pkg==2.0.0), but not < or >.
// If different lambdas import different versions of the same package,
// we will install them, for example, to /packages/pkg==1.0.0/pkg and
// /packages/pkg==2.0.0/pkg.  We'll symlink the version the user wants
// to /handler/packages/pkg.  For example, two different lambdas might
// have links as follows:
//
// /handler/packages/pkg => /packages/pkg==1.0.0/pkg
// /handler/packages/pkg => /packages/pkg==2.0.0/pkg
//
// Lambdas should have /handler/packages in their path, but not
// /packages.
func parseMeta(codeDir string) (meta *sandbox.SandboxMeta, err error) {
	installs := make([]string, 0)
	imports := make([]string, 0)
	var timeout_time int64 = 0

	path := filepath.Join(codeDir, "f.py")
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	scnr := bufio.NewScanner(file)
	for scnr.Scan() {
		line := strings.ReplaceAll(scnr.Text(), " ", "")
		parts := strings.Split(line, ":")

		// Check to make sure that we don't go out of bounds.
		// If not enough arguments specified, then just ignore the OpenLambda Directive...
		if len(parts) == 2 {
			if parts[0] == "#ol-install" {
				for _, val := range strings.Split(parts[1], ",") {
					val = strings.TrimSpace(val)
					if len(val) > 0 {
						installs = append(installs, val)
					}
				}
			} else if parts[0] == "#ol-import" {
				for _, val := range strings.Split(parts[1], ",") {
					val = strings.TrimSpace(val)
					if len(val) > 0 {
						imports = append(imports, val)
					}
				}
			} else if parts[0] == "#ol-timeout" {

				const BASE_TEN = 10
				const BITS_64 = 64
					res, err := strconv.ParseInt(parts[1], BASE_TEN, BITS_64)
					if err == nil {
						timeout_time = res
					} else {
						fmt.Printf("WARNING: Malformed floating point value detected for #ol-timeout\n")
						fmt.Printf("#ol-timeout will be ignored for the affected lambda.\n")
					}

			}
		} else {
			fmt.Printf("WARNING: Incorrect format specified for metadata in %s. It will be ignored as a consequence.\n", codeDir)
			fmt.Printf("Expected format #ol-timeout:[timeout time in milliseconds]\n")
		}
	}

	for i, pkg := range installs {
		installs[i] = normalizePkg(pkg)
	}

	return &sandbox.SandboxMeta{
		Installs:     installs,
		Imports:      imports,
		Timeout_Time: timeout_time,
	}, nil
}

// if there is any error:
// 1. we won't switch to the new code
// 2. we won't update pull time (so well check for a fix next tim)
func (f *LambdaFunc) pullHandlerIfStale() (err error) {
	// check if there is newer code, download it if necessary
	now := time.Now()
	cache_ns := int64(common.Conf.Registry_cache_ms) * 1000000

	// should we check for new code?
	if f.lastPull != nil && int64(now.Sub(*f.lastPull)) < cache_ns {
		return nil
	}

	// is there new code?
	codeDir, err := f.lmgr.HandlerPuller.Pull(f.name)
	if err != nil {
		return err
	}

	if codeDir == f.codeDir {
		return nil
	}

	defer func() {
		if err != nil {
			if err := os.RemoveAll(codeDir); err != nil {
				log.Printf("could not cleanup %s after failed pull", codeDir)
			}

			// we dirty this dir (e.g., by setting up
			// symlinks to packages, so we want the
			// HandlerPuller to give us a new one next
			// time, even if the code hasn't changed
			f.lmgr.HandlerPuller.Reset(f.name)
		}
	}()

	// inspect new code for dependencies; if we can install
	// everything necessary, start using new code
	meta, err := parseMeta(codeDir)
	if err != nil {
		return err
	}

	meta.Installs, err = f.lmgr.PackagePuller.InstallRecursive(meta.Installs)
	if err != nil {
		return err
	}
	f.lmgr.DepTracer.TraceFunction(codeDir, meta.Installs)

	f.codeDir = codeDir
	f.meta = meta
	f.lastPull = &now
	return nil
}

// this Task receives lambda requests, fetches new lambda code as
// needed, and dispatches to a set of lambda instances.  Task also
// monitors outstanding requests, and scales the number of instances
// up or down as needed.
//
// communication for a given request is as follows (each of the four
// transfers are commented within the function):
//
// client -> function -> instance -> function -> client
//
// each of the 4 handoffs above is over a chan.  In order, those chans are:
// 1. LambdaFunc.funcChan
// 2. LambdaFunc.instChan
// 3. LambdaFunc.doneChan
// 4. Invocation.done
//
// If either LambdaFunc.funcChan or LambdaFunc.instChan is full, we
// respond to the client with a backoff message: StatusTooManyRequests
func (f *LambdaFunc) Task() {
	f.printf("debug: LambdaFunc.Task() runs on goroutine %d", common.GetGoroutineID())

	// we want to perform various cleanup actions, such as killing
	// instances and deleting old code.  We want to do these
	// asyncronously, but in order.  Thus, we use a chan to get
	// FIFO behavior and a single cleanup task to get async.
	//
	// two types can be sent to this chan:
	//
	// 1. string: this is a path to be deleted
	//
	// 2. chan: this is a signal chan that corresponds to
	// previously initiated cleanup work.  We block until we
	// receive the complete signal, before proceeding to
	// subsequent cleanup tasks in the FIFO.
	cleanupChan := make(chan interface{}, 32)
	cleanupTaskDone := make(chan bool)
	go func() {
		for {
			msg, ok := <-cleanupChan
			if !ok {
				cleanupTaskDone <- true
				return
			}

			switch op := msg.(type) {
			case string:
				if err := os.RemoveAll(op); err != nil {
					f.printf("Async code cleanup could not delete %s, even after all instances using it killed: %v", op, err)
				}
			case chan bool:
				<-op
			}
		}
	}()

	// stats for autoscaling
	outstandingReqs := 0
	execMs := common.NewRollingAvg(10)
	var lastScaling *time.Time = nil
	timeout := time.NewTimer(0)

	for {
		select {
		case <-timeout.C:
			if f.codeDir == "" {
				continue
			}
		case req := <-f.funcChan:
			// msg: client -> function

			// check for new code, and cleanup old code
			// (and instances that use it) if necessary
			oldCodeDir := f.codeDir
			if err := f.pullHandlerIfStale(); err != nil {
				f.printf("Error checking for new lambda code: %v", err)
				req.w.WriteHeader(http.StatusInternalServerError)
				req.w.Write([]byte(err.Error() + "\n"))
				req.done <- true
				continue
			}

			if oldCodeDir != "" && oldCodeDir != f.codeDir {
				el := f.instances.Front()
				for el != nil {
					waitChan := el.Value.(*LambdaInstance).AsyncKill()
					cleanupChan <- waitChan
					el = el.Next()
				}
				f.instances = list.New()

				// cleanupChan is a FIFO, so this will
				// happen after the cleanup task waits
				// for all instance kills to finish
				cleanupChan <- oldCodeDir
			}

			f.lmgr.DepTracer.TraceInvocation(f.codeDir)

			select {
			case f.instChan <- req:
				// msg: function -> instance
				outstandingReqs += 1
			default:
				// queue cannot accept more, so reply with backoff
				req.w.WriteHeader(http.StatusTooManyRequests)
				req.w.Write([]byte("lambda instance queue is full"))
				req.done <- true
			}
		case req := <-f.doneChan:
			// msg: instance -> function

			execMs.Add(req.execMs)
			outstandingReqs -= 1

			// msg: function -> client
			req.done <- true

		case done := <-f.killChan:
			// signal all instances to die, then wait for
			// cleanup task to finish and exit
			el := f.instances.Front()
			for el != nil {
				waitChan := el.Value.(*LambdaInstance).AsyncKill()
				cleanupChan <- waitChan
				el = el.Next()
			}
			if f.codeDir != "" {
				//cleanupChan <- f.codeDir
			}
			close(cleanupChan)
			<-cleanupTaskDone
			done <- true
			return
		}

		// POLICY: how many instances (i.e., virtual sandboxes) should we allocate?

		// AUTOSCALING STEP 1: decide how many instances we want

		// let's aim to have 1 sandbox per second of outstanding work
		inProgressWorkMs := outstandingReqs * execMs.Avg
		desiredInstances := inProgressWorkMs / 1000

		// if we have, say, one job that will take 100
		// seconds, spinning up 100 instances won't do any
		// good, so cap by number of outstanding reqs
		if outstandingReqs < desiredInstances {
			desiredInstances = outstandingReqs
		}

		// always try to have one instance
		if desiredInstances < 1 {
			desiredInstances = 1
		}

		// AUTOSCALING STEP 2: tweak how many instances we have, to get closer to our goal

		// make at most one scaling adjustment per second
		adjustFreq := time.Second
		now := time.Now()
		if lastScaling != nil {
			elapsed := now.Sub(*lastScaling)
			if elapsed < adjustFreq {
				if desiredInstances != f.instances.Len() {
					timeout = time.NewTimer(adjustFreq - elapsed)
				}
				continue
			}
		}

		// kill or start at most one instance to get closer to
		// desired number
		if f.instances.Len() < desiredInstances {
			f.printf("increase instances to %d", f.instances.Len()+1)
			f.newInstance()
			lastScaling = &now
		} else if f.instances.Len() > desiredInstances {
			f.printf("reduce instances to %d", f.instances.Len()-1)
			waitChan := f.instances.Back().Value.(*LambdaInstance).AsyncKill()
			f.instances.Remove(f.instances.Back())
			cleanupChan <- waitChan
			lastScaling = &now
		}

		if f.instances.Len() != desiredInstances {
			// we can only adjust quickly, so we want to
			// run through this loop again as soon as
			// possible, even if there are no requests to
			// service.
			timeout = time.NewTimer(adjustFreq)
		}
	}
}

func (f *LambdaFunc) newInstance() {
	if f.codeDir == "" {
		panic("cannot start instance until code has been fetched")
	}

	linst := &LambdaInstance{
		lfunc:    f,
		codeDir:  f.codeDir,
		meta:     f.meta,
		killChan: make(chan chan bool, 1),
	}

	f.instances.PushBack(linst)

	go linst.Task()
}

func (f *LambdaFunc) Kill() {
	done := make(chan bool)
	f.killChan <- done
	<-done
}

// this Task manages a single Sandbox (at any given time), and
// forwards requests from the function queue to that Sandbox.
// when there are no requests, the Sandbox is paused.
//
// These errors are handled as follows by Task:
//
// 1. Sandbox.Pause/Unpause: discard Sandbox, create new one to handle request
// 2. Sandbox.Create/Channel: discard Sandbox, propagate HTTP 500 to client
// 3. Error inside Sandbox: simply propagate whatever occured to client (TODO: restart Sandbox)
func (linst *LambdaInstance) Task() {
	f := linst.lfunc

	var sb sandbox.Sandbox = nil
	//var client *http.Client = nil // whenever we create a Sandbox, we init this too
	var err error

	for {
		// wait for a request (blocking) before making the
		// Sandbox ready, or kill if we receive that signal
		var req *Invocation
		select {
		case req = <-f.instChan:
		case killed := <-linst.killChan:
			if sb != nil {
				sb.Destroy()
			}
			killed <- true
			return
		}

		// if we have a sandbox, try unpausing it to see if it is still alive
		if sb != nil {
			// Unpause will often fail, because evictors
			// are likely to prefer to evict paused
			// sandboxes rather than inactive sandboxes.
			// Thus, if this fails, we'll try to handle it
			// by just creating a new sandbox.
			if err := sb.Unpause(); err != nil {
				f.printf("discard sandbox %s due to Unpause error: %v", sb.ID(), err)
				sb = nil
			}
		}

		// if we don't already have a Sandbox, create one, and
		// HTTP proxy over the channel
		if sb == nil {
			sb = nil
			if f.lmgr.ImportCache != nil {
				scratchDir := f.lmgr.scratchDirs.Make(f.name)

				// we don't specify parent SB, because ImportCache.Create chooses it for us
				sb, err = f.lmgr.ImportCache.Create(f.lmgr.sbPool, true, linst.codeDir, scratchDir, linst.meta)
				if err != nil {
					f.printf("failed to get Sandbox from import cache")
					sb = nil
				}
			}

			// import cache is either disabled or it failed
			if sb == nil {
				scratchDir := f.lmgr.scratchDirs.Make(f.name)
				sb, err = f.lmgr.sbPool.Create(nil, true, linst.codeDir, scratchDir, linst.meta)
			}

			if err != nil {
				req.w.WriteHeader(http.StatusInternalServerError)
				req.w.Write([]byte("could not create Sandbox: " + err.Error() + "\n"))
				f.doneChan <- req
				continue // wait for another request before retrying
			}

			if err != nil {
				req.w.WriteHeader(http.StatusInternalServerError)
				req.w.Write([]byte("could not connect to Sandbox: " + err.Error() + "\n"))
				f.doneChan <- req
				f.printf("discard sandbox %s due to Channel error: %v", sb.ID(), err)
				sb = nil
				continue // wait for another request before retrying
			}
		}

		// below here, we're guaranteed (1) sb != nil, (2) sb is unpaused

		// serve until we incoming queue is empty
		for req != nil {
			// ask Sandbox to respond, via HTTP proxy
			t := common.T0("ServeHTTP")
			var tb TimeoutBroker
			const NANOSEC_PER_MS = 1000000
			var chosen_timeout int64

			default_timeout := common.Conf.Limits.Max_timeout_ms
			override_timeout := linst.meta.Timeout_Time

			// Resolve timeout:
			// In general, use the override timeout if it is lower than the default timeout. Otherwise, use the default timeout
			// An exception is if the default timeout is <=0... then always use the override timeout
			// Another exception (second precedence) is if the override timeout is <=0... then use the default timeout
			if default_timeout <= 0 {
				chosen_timeout = override_timeout
			} else if override_timeout <= 0 {
				chosen_timeout = default_timeout
			} else if override_timeout < default_timeout {
				chosen_timeout = override_timeout
			} else {
				chosen_timeout = default_timeout
			}

			var conf_to_sec time.Duration = time.Duration(chosen_timeout * NANOSEC_PER_MS)

			// Set timed out signal to false by default, invalid signal
			tb.timedout = false
			tb.timerinvalid = false

			// case: timeout time is greater than 0, use it and start the timeout timer
			// if it's not, then just ignore it (i.e. timeout is disabled)
			if IsFiniteTimeout(chosen_timeout) {
				ct, cf := context.WithTimeout(req.r.Context(), conf_to_sec)
				tb.suicideTimer = time.AfterFunc(conf_to_sec, tb.CloseInstance)
				tb.linst = linst
				tb.cancel = cf
				req.r = req.r.WithContext(ct)
			}

			sb.SendRequest(&req.w, req.r)

			if IsFiniteTimeout(chosen_timeout) {
				tb.destlock.Lock()
				tb.timerinvalid = true
				tb.suicideTimer.Stop() // If request finishes, then shouldn't mark for del.
				tb.destlock.Unlock()
			}

			if tb.timedout {
				sb.Destroy() // Garbage collect sandbox state
				req.w.Write([]byte("ERROR: Lambda took too long to respond, and has timed out.\n"))
			}

			t.T1()
			req.execMs = int(t.Milliseconds)
			f.doneChan <- req

			// check whether we should shutdown (non-blocking)
			select {
			case killed := <-linst.killChan:
				sb.Destroy()
				killed <- true
				return
			default:
			}

			// grab another request (non-blocking)
			select {
			case req = <-f.instChan:
			default:
				req = nil
			}
		}

		if err := sb.Pause(); err != nil {
			f.printf("discard sandbox %s due to Pause error: %v", sb.ID(), err)
			sb = nil
		}
	}
}

// signal the instance to die, return chan that can be used to block
// until it's done
func (linst *LambdaInstance) AsyncKill() chan bool {
	done := make(chan bool)
	linst.killChan <- done
	return done
}

// Wrapper to AsyncKill- a function explicitly for causing a lambda function
// to self destruct
func (tb *TimeoutBroker) CloseInstance() {

	tb.destlock.Lock()
	if !tb.timerinvalid {
		fmt.Printf("WARNING: A lambda instance has timed out, and will now end itself.\n")
		tb.timerinvalid = true
		tb.suicideTimer.Stop()

		// Set destruction bool
		tb.timedout = true

		// Cancel the current running request
		tb.cancel()
		fmt.Printf("INFO: Clean up for lambda instance engaged...\n")
	}
	tb.destlock.Unlock()

}

// Predicate Function which checks if the inputted timeout is valid
func IsFiniteTimeout(to int64) bool {
	return to > 0
}
