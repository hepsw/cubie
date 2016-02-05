// cubied manages processes as per the cubie configuration manifest.
package main

import (
	"encoding/json"
	"flag"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// PR_SET_CHILD_SUBREAPER is defined in <sys/prctl.h> for linux >= 3.4
const PR_SET_CHILD_SUBREAPER = 36

var (
	confDir = flag.String("conf", "/etc/cubie.d", "directory holding cubie configuration files")

	ctl = NewControl()
)

func main() {
	flag.Parse()

	log.Printf("::: starting cubie-daemon [%v]...\n", Version)

	if os.Getpid() != 1 {
		// try to register as a subreaper
		err := unix.Prctl(PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0)
		log.Printf("subreaper: %v\n", err == nil)
	}

	confs, err := filepath.Glob(filepath.Join(*confDir, "*.conf"))
	if err != nil {
		log.Fatalf("could not collect configuration files: %v\n", err)
	}

	for _, fname := range confs {
		f, err := os.Open(fname)
		if err != nil {
			log.Fatalf("could not open configuration file [%s]: %v\n", fname, err)
		}
		defer f.Close()

		var proc Process
		err = json.NewDecoder(f).Decode(&proc)
		if err != nil {
			log.Fatalf(
				"could not decode configuration file [%s]: %v\n",
				fname,
				err,
			)
		}

		ctl.procs = append(ctl.procs, &proc)
	}

	if flag.NArg() > 0 {
		ctl.cmd = newProcess(flag.Args()...)
	}

	go func() {
		sigch := make(chan os.Signal)
		signal.Notify(sigch, os.Interrupt, os.Kill)
		for {
			select {
			case <-sigch:
				err = ctl.killProcs()
				if err != nil {
					log.Fatalf("error killing managed processes: %v\n", err)
				}
			}
		}
	}()

	ctl.run()

	os.Exit(0)
}

type procKeyType int

var procKey procKeyType = 0

// Process represents a process to be launched and how it needs to be launched.
type Process struct {
	Cmd    string            // Cmd is the name of the command to be launched
	Args   []string          // Args holds command-line arguments, excluding the command name
	Env    map[string]string // Env specifies the environment of the command
	Dir    string            // Dir specifies the working directory of the command
	Daemon bool

	proc *exec.Cmd
	err  error
	quit chan struct{}
}

// newProcess creates a new Process from a command and its (optional) arguments.
func newProcess(cmd ...string) *Process {
	proc := &Process{
		Cmd: cmd[0],
	}

	if len(cmd) > 1 {
		proc.Args = append(proc.Args, cmd[1:]...)
	}

	return proc
}

func (p *Process) title() string {
	return strings.Join(p.proc.Args, " ")
}

func (p *Process) wait() error {
	var err1 error
	var status syscall.WaitStatus
	_, err1 = syscall.Wait4(p.proc.Process.Pid, &status, syscall.WALL|0, nil)
	return err1
}

func (p *Process) run(done chan *Process) {
	p.proc.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: true,
	}

	defer func() { done <- p }()
	log.Printf("starting [%s]...\n", p.title())
	p.err = p.proc.Start()
	if p.err != nil {
		return
	}

	p.err = p.wait()
	log.Printf("running [%s]... [done]\n", p.title())
	return
}

// Control runs and manages processes.
type Control struct {
	pid   int
	cmd   *Process
	procs []*Process
	quit  chan struct{}
}

// NewControl creates a Control handle.
func NewControl() *Control {
	ctl := &Control{
		pid:   os.Getpid(),
		procs: make([]*Process, 0, 2),
		quit:  make(chan struct{}),
	}
	return ctl
}

// run runs all processes according to cubie configuration.
func (ctl *Control) run() {
	n := 0
	for _, p := range ctl.procs {
		if !p.Daemon {
			n++
		}
	}

	var wg sync.WaitGroup
	wg.Add(n)
	nprocs := ""
	if n > 1 {
		nprocs = "es"
	}
	log.Printf("starting %d go-process%s...\n", n, nprocs)

	go ctl.runProcs(&wg)
	wg.Wait()

	log.Printf("now running main go-process...\n")
	ctl.runCmd()
}

func (ctl *Control) setupProc(proc *Process) {
	proc.proc = exec.Command(proc.Cmd, proc.Args...)
	if len(proc.Env) > 0 {
		env := make([]string, 0, len(proc.Env))
		for k, v := range proc.Env {
			env = append(env, k+"="+v)
		}
		proc.proc.Env = env
	}

	if proc.Dir != "" {
		proc.proc.Dir = proc.Dir
	}

	proc.proc.Stdout = os.Stdout
	proc.proc.Stderr = os.Stderr
	proc.proc.Stdin = os.Stdin
}

func (ctl *Control) runProcs(wg *sync.WaitGroup) {

	done := make(chan *Process)
	for _, p := range ctl.procs {
		ctl.setupProc(p)
		go p.run(done)
	}

	sigch := make(chan os.Signal)
	signal.Notify(sigch, syscall.SIGCHLD)

reaploop:
	for {
		select {
		case <-ctl.quit:
			ctl.killProcs()
			return

		case <-sigch:
			const nretries = 1000
			var ws syscall.WaitStatus
			for i := 0; i < nretries; i++ {
				pid, err := syscall.Wait4(ctl.pid, &ws, syscall.WNOHANG, nil)
				// pid > 0 => pid is the ID of the child that died, but
				//  there could be other children that are signalling us
				//  and not the one we in particular are waiting for.
				// pid -1 && errno == ECHILD => no new status children
				// pid -1 && errno != ECHILD => syscall interupped by signal
				// pid == 0 => no more children to wait for.
				switch {
				case err != nil:
					continue reaploop
				case pid == ctl.pid:
					return
				case pid == 0:
					// this is what we get when SIGSTOP is sent on OSX. ws == 0 in this case.
					// Note that on OSX we never get a SIGCONT signal.
					// Under WNOHANG, pid == 0 means there is nobody left to wait for,
					// so just go back to waiting for another SIGCHLD.
					continue reaploop
				default:
					time.Sleep(time.Millisecond)
				}
			}
			log.Fatalf("failed to reap children after [%d] retries\n", nretries)

		case p := <-done:
			i := -1
			for j, pp := range ctl.procs {
				if pp == p {
					i = j
					break
				}
			}
			if i < 0 {
				panic("impossible")
			}

			nprocs := len(ctl.procs)
			ctl.procs[nprocs-1], ctl.procs = nil, append(ctl.procs[:i], ctl.procs[i+1:]...)
			ctl.procs = ctl.procs[:len(ctl.procs):len(ctl.procs)]
			wg.Done()
			if p.err != nil {
				ctl.killProcs()
				log.Fatalf("error running process [%v]: %v\n", p.title(), p.err)
			}
			if len(ctl.procs) <= 0 {
				return
			}
		}
	}
}

func (ctl *Control) runCmd() {
	if ctl.cmd == nil {
		return
	}
	done := make(chan *Process)
	ctl.setupProc(ctl.cmd)
	go ctl.cmd.run(done)

	for {
		select {
		case <-ctl.quit:
			ctl.killProcs()
			return

		case p := <-done:
			if p.err != nil {
				ctl.killProcs()
				log.Fatalf("error running process [%v]: %v\n", p.title(), p.err)
			}
			return
		}
	}
}

func (ctl *Control) killProcs() error {
	var err error
	for _, p := range ctl.procs {
		if p.proc == nil {
			continue
		}
		errp := killProc(p.proc.Process)
		if errp != nil && err != nil {
			err = errp
		}
	}
	if ctl.cmd != nil && ctl.cmd.proc != nil {
		errp := killProc(ctl.cmd.proc.Process)
		if err != nil && err != nil {
			err = errp
		}
	}
	return err
}

func killProc(p *os.Process) error {
	if p == nil {
		return nil
	}
	pgid, err := syscall.Getpgid(p.Pid)
	if err != nil {
		return err
	}
	err = syscall.Kill(-pgid, syscall.SIGKILL) // note the minus sign
	return err
}
