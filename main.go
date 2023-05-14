// Code maintained by the OpenMandriva Association
// Original code copyright (c) 2014-2019 Ludovic Fauvet
// Licensed under the MIT license
// Maintained at https://github.com/OpenMandrivaSoftware/mirrorbits

package main

import (
	"fmt"
	"os"
	"os/signal"
	"runtime/pprof"
	"strings"
	"syscall"
	"time"

	"github.com/OpenMandrivaSoftware/mirrorbits/cli"
	. "github.com/OpenMandrivaSoftware/mirrorbits/config"
	"github.com/OpenMandrivaSoftware/mirrorbits/core"
	"github.com/OpenMandrivaSoftware/mirrorbits/daemon"
	"github.com/OpenMandrivaSoftware/mirrorbits/database"
	"github.com/OpenMandrivaSoftware/mirrorbits/http"
	"github.com/OpenMandrivaSoftware/mirrorbits/logs"
	"github.com/OpenMandrivaSoftware/mirrorbits/mirrors"
	"github.com/OpenMandrivaSoftware/mirrorbits/process"
	"github.com/OpenMandrivaSoftware/mirrorbits/rpc"
	"github.com/op/go-logging"
	"github.com/pkg/errors"
)

var (
	log = logging.MustGetLogger("main") // Get the logger for main package
)

func main() {
	core.Parseflags() // Parse command line flags

	// Handle CPU profiling if enabled
	if core.CpuProfile != "" {
		f, err := os.Create(core.CpuProfile)
		if err != nil {
			log.Fatal(err)
		}
		defer f.Close()
		pprof.StartCPUProfile(f)
		defer pprof.StopCPUProfile()
	}

	// If the application is running as a daemon
	if core.Daemon {
		LoadConfig() // Load the application config
		logs.ReloadLogs() // Reload the logs

		process.WritePidFile() // Write the process ID to a file

		// Show welcome logo
		fmt.Printf(core.Banner+"\n\n", core.VERSION)

		// Setup RPC and handle errors
		rpcs := new(rpc.CLI)
		if err := rpcs.Start(); err != nil {
			log.Fatal(errors.Wrap(err, "rpc error"))
		}

		// Connect to the database
		r := database.NewRedis()
		r.ConnectPubsub()
		rpcs.SetDatabase(r)
		c := mirrors.NewCache(r)
		rpcs.SetCache(c)
		h := http.HTTPServer(r, c)

		// Start the background monitor
		m := daemon.NewMonitor(r, c)
		if core.Monitor {
			go m.MonitorLoop()
		}

		// Handle system signals
		handleSignals(rpcs, m, h)

		// Recover an existing listener (see process.go)
		if l, ppid, err := process.Recover(); err == nil {
			h.SetListener(l)
			go func() {
				time.Sleep(100 * time.Millisecond)
				process.KillParent(ppid)
			}()
		}

		// Start the HTTP server
		runHTTPServer(h)

		log.Debug("Waiting for monitor termination")
		m.Wait()

		log.Debug("Terminating server")
		h.Terminate()

		r.Close()

		process.RemovePidFile()
	} else {
		// Handle command line interface arguments
	args := os.Args[len(os.Args)-core.NArg:]
		if err := cli.ParseCommands(args...); err != nil {
			fmt.Fprintf(os.Stderr, "%s\n", err)
			os.Exit(1)
		}
	}
	os.Exit(0)
}

// handleSignals deals with the different system signals that the application might receive
func handleSignals(rpcs *rpc.CLI, m *daemon.Monitor, h *http.HTTP) {
	k := make(chan os.Signal, 1)
	rpcs.SetSignals(k)
	signal.Notify(k,
		syscall.SIGINT,  // Terminate
		syscall.SIGTERM, // Terminate
		syscall.SIGQUIT, // Stop gracefully
		syscall.SIGHUP,  // Reload config
		syscall.SIGUSR1, // Reopen log files
		syscall.SIGUSR2, // Seamless binary upgrade
	)
	go func() {
		for {
			sig := <-k
			switch sig {
			case syscall.SIGINT, syscall.SIGTERM:
				process.RemovePidFile()
				os.Exit(0)
			case syscall.SIGQUIT:
				stopGracefully(rpcs, m, h)
			case syscall.SIGHUP:
				handleSIGHUP(h)
			case syscall.SIGUSR1:
				log.Notice("SIGUSR1 Received: Re-opening logs...")
				logs.ReloadLogs()
			case syscall.SIGUSR2:
				handleSIGUSR2(rpcs, h)
			}
		}
	}()
}

// stopGracefully handles the graceful shutdown of the application
func stopGracefully(rpcs *rpc.CLI, m *daemon.Monitor, h *http.HTTP) {
	m.Stop()
	rpcs.Close()
	if h.Listener != nil {
		log.Notice("Waiting for running tasks to finish...")
		h.Stop(5 * time.Second)
	} else {
		process.RemovePidFile()
		os.Exit(0)
	}
}

// handleSIGHUP deals with the SIGHUP signal for reloading the config
func handleSIGHUP(h *http.HTTP) {
	listenAddress := GetConfig().ListenAddress
	if err := ReloadConfig(); err != nil {
		log.Warningf("SIGHUP Received: %s\n", err)
	} else {
		log.Notice("SIGHUP Received: Reloading configuration...")
	}
	if GetConfig().ListenAddress != listenAddress {
		h.Restarting = true
		h.Stop(1 * time.Second)
	}
	h.Reload()
}

// handleSIGUSR2 deals with the SIGUSR2 signal for a seamless binary upgrade
func handleSIGUSR2(rpcs *rpc.CLI, h *http.HTTP) {
	log.Notice("SIGUSR2 Received: Seamless binary upgrade...")
	rpcs.Close()
	err := process.Relaunch(*h.Listener)
	if err != nil {
		log.Errorf("Relaunch failed: %s\n", err)
	}
}

// runHTTPServer starts the HTTP server and handles errors during runtime
func runHTTPServer(h *http.HTTP) {
	var err error
	for {
		err = h.RunServer()
		if h.Restarting {
			h.Restarting = false
			continue
		}
		// This check is ugly but there's still no way to detect this error by type
		if err != nil && strings.Contains(err.Error(), "use of closed network connection") {
			// This error is expected during a graceful shutdown
			err = nil
		}
		break
	}

	if err != nil {
		log.Fatal(err)
	} else {
		log.Notice("Server stopped gracefully.")
	}
}
