package tenderduty

import (
	"encoding/json"
	"fmt"
	dash "github.com/blockpane/tenderduty/v2/td2/dashboard"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"
)

var td = &Config{}

func Run(configFile, stateFile string, dumpConfig bool) error {
	var err error
	td, err = loadConfig(configFile, stateFile, dumpConfig)
	if err != nil {
		return err
	}
	fatal, problems := validateConfig(td)
	for _, p := range problems {
		fmt.Println(p)
	}
	if fatal {
		log.Fatal("tenderduty the configuration is invalid, refusing to start")
	}
	log.Println("tenderduty config is valid, starting tenderduty with", len(td.Chains), "chains")

	defer td.cancel()

	go func() {
		for {
			select {
			case alert := <-td.alertChan:
				go func(msg *alertMsg) {
					var e error
					e = notifyPagerduty(msg)
					if e != nil {
						l(msg.chain, "error sending alert to pagerduty", e.Error())
					}
					e = notifyDiscord(msg)
					if e != nil {
						l(msg.chain, "error sending alert to discord", e.Error())
					}
					e = notifyTg(msg)
					if e != nil {
						l(msg.chain, "error sending alert to telegram", e.Error())
					}
				}(alert)
			case <-td.ctx.Done():
				return
			}
		}
	}()

	if td.EnableDash {
		l("starting dashboard on", td.Listen)
		go dash.Serve(td.Listen, td.updateChan, td.logChan, td.HideLogs)
	} else {
		go func() {
			for {
				_ = <-td.updateChan
			}
		}()
	}
	if td.Prom {
		go prometheusExporter(td.ctx, td.statsChan)
	} else {
		go func() {
			for {
				_ = <-td.statsChan
			}
		}()
	}

	for k := range td.Chains {
		cc := td.Chains[k]

		go func(cc *ChainConfig, name string) {
			// alert worker
			go cc.watch()

			// node health checks:
			go func() {
				for {
					time.Sleep(time.Minute)
					cc.monitorHealth(td.ctx, name)
				}
			}()

			// websocket subscription and occasional validator info refreshes
			for {
				e := cc.newRpc()
				if e != nil {
					l(cc.ChainId, e)
					time.Sleep(5 * time.Second)
					continue
				}
				e = cc.GetValInfo(true)
				if e != nil {
					l("🛑", cc.ChainId, e)
				}
				cc.WsRun()
				l(cc.ChainId, "🌀 websocket exited! Restarting monitoring")
				time.Sleep(5 * time.Second)
			}
		}(cc, k)
	}

	// attempt to save state on exit, only a best-effort ...
	saved := make(chan interface{})
	go saveOnExit(stateFile, saved)

	<-td.ctx.Done()
	<-saved

	return err
}

func saveOnExit(stateFile string, saved chan interface{}) {
	quitting := make(chan os.Signal, 1)
	signal.Notify(quitting, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGKILL)

	saveState := func() {
		defer close(saved)
		log.Println("saving state...")
		//#nosec -- variable specified on command line
		f, e := os.OpenFile(stateFile, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
		if e != nil {
			log.Println(e)
			return
		}
		td.chainsMux.Lock()
		defer td.chainsMux.Unlock()
		blocks := make(map[string][]int)
		// only need to save counts if the dashboard exists
		if td.EnableDash {
			for k, v := range td.Chains {
				blocks[k] = v.blocksResults
			}
		}
		b, e := json.Marshal(&savedState{
			Alarms: alarms,
			Blocks: blocks,
		})
		if e != nil {
			log.Println(e)
			return
		}
		_, _ = f.Write(b)
		_ = f.Close()
		log.Println("tenderduty exiting.")
	}
	for {
		select {
		case <-td.ctx.Done():
			saveState()
			return
		case <-quitting:
			saveState()
			td.cancel()
			return
		}
	}
}
