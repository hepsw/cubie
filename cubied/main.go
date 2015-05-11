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
	"syscall"
)

var (
	confDir = flag.String("conf", "/etc/cubie.d", "directory holding cubie configuration files")

	ctl = NewControl()
)

func main() {
	flag.Parse()

	log.Printf("::: starting cubie-daemon [%v]...\n", Version)

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
		ctl.procs = append(ctl.procs, newProcess(flag.Args()...))
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
	Cmd  string            // Cmd is the name of the command to be launched
	Args []string          // Args holds command-line arguments, excluding the command name
	Env  map[string]string // Env specifies the environment of the command
	Dir  string            // Dir specifies the working directory of the command

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

func (p *Process) run(done chan *Process) {
	defer func() { done <- p }()
	log.Printf("starting [%s]...\n", p.title())
	p.err = p.proc.Start()
	if p.err != nil {
		return
	}

	p.err = p.proc.Wait()
	return
}

// Control runs and manages processes.
type Control struct {
	procs []*Process
	quit  chan struct{}
}

// NewControl creates a Control handle.
func NewControl() *Control {
	ctl := &Control{
		procs: make([]*Process, 0, 2),
		quit:  make(chan struct{}),
	}
	return ctl
}

// run runs all processes according to cubie configuration.
func (ctl *Control) run() {
	for i := range ctl.procs {
		proc := ctl.procs[i]
		proc.proc = exec.Command(proc.Cmd, proc.Args...)
		if len(proc.Env) > 0 {
			env := make([]string, 0, len(proc.Env))
			for k, v := range proc.Env {
				env = append(env, k+"="+v)
			}
			proc.proc.Env = env
			log.Printf("env[%s]: %v\n", proc.title(), env)
		}

		if proc.Dir != "" {
			proc.proc.Dir = proc.Dir
		}
		// create a process group for this process.
		proc.proc.SysProcAttr = &syscall.SysProcAttr{
			Setpgid: true,
		}

		proc.proc.Stdout = os.Stdout
		proc.proc.Stderr = os.Stderr
		proc.proc.Stdin = os.Stdin
	}

	done := make(chan *Process)
	for _, p := range ctl.procs {
		go p.run(done)
	}

	for {
		select {
		case <-ctl.quit:
			ctl.killProcs()
			return

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

func (ctl *Control) killProcs() error {
	var err error
	for _, p := range ctl.procs {
		errp := killProc(p.proc.Process)
		if errp != nil && err != nil {
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
