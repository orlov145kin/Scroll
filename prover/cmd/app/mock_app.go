package app

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"golang.org/x/sync/errgroup"

	rollerConfig "scroll-tech/prover/config"

	"scroll-tech/common/cmd"
	"scroll-tech/common/docker"
	"scroll-tech/common/utils"
)

var (
	rollerIndex int
)

func getIndex() int {
	defer func() { rollerIndex++ }()
	return rollerIndex
}

// ProverApp prover-test client manager.
type ProverApp struct {
	Config *rollerConfig.Config

	base *docker.App

	originFile string
	rollerFile string
	bboltDB    string

	index int
	name  string
	args  []string
	docker.AppAPI
}

// NewProverApp return a new rollerApp manager.
func NewProverApp(base *docker.App, file string, wsURL string) *ProverApp {
	rollerFile := fmt.Sprintf("/tmp/%d_roller-config.json", base.Timestamp)
	rollerApp := &ProverApp{
		base:       base,
		originFile: file,
		rollerFile: rollerFile,
		bboltDB:    fmt.Sprintf("/tmp/%d_bbolt_db", base.Timestamp),
		index:      getIndex(),
		name:       string(utils.ProverApp),
		args:       []string{"--log.debug", "--config", rollerFile},
	}
	if err := rollerApp.MockConfig(true, wsURL); err != nil {
		panic(err)
	}
	return rollerApp
}

// RunApp run prover-test child process by multi parameters.
func (r *ProverApp) RunApp(t *testing.T, args ...string) {
	r.AppAPI = cmd.NewCmd(r.name, append(r.args, args...)...)
	r.AppAPI.RunApp(func() bool { return r.AppAPI.WaitResult(t, time.Second*40, "prover start successfully") })
}

// Free stop and release prover-test.
func (r *ProverApp) Free() {
	if !utils.IsNil(r.AppAPI) {
		r.AppAPI.WaitExit()
	}
	_ = os.Remove(r.rollerFile)
	_ = os.Remove(r.Config.KeystorePath)
	_ = os.Remove(r.bboltDB)
}

// MockConfig creates a new prover config.
func (r *ProverApp) MockConfig(store bool, wsURL string) error {
	cfg, err := rollerConfig.NewConfig(r.originFile)
	if err != nil {
		return err
	}
	cfg.RollerName = fmt.Sprintf("%s_%d", r.name, r.index)
	cfg.KeystorePath = fmt.Sprintf("/tmp/%d_%s.json", r.base.Timestamp, cfg.RollerName)
	cfg.TraceEndpoint = r.base.L2gethImg.Endpoint()
	// Reuse l1geth's keystore file
	cfg.KeystorePassword = "scrolltest"
	cfg.DBPath = r.bboltDB
	// Create keystore file.
	_, err = utils.LoadOrCreateKey(cfg.KeystorePath, cfg.KeystorePassword)
	if err != nil {
		return err
	}
	cfg.CoordinatorURL = wsURL
	r.Config = cfg

	if !store {
		return nil
	}

	data, err := json.Marshal(r.Config)
	if err != nil {
		return err
	}
	return os.WriteFile(r.rollerFile, data, 0600)
}

// RollerApps rollerApp list.
type RollerApps []*ProverApp

// RunApps starts all the rollerApps.
func (r RollerApps) RunApps(t *testing.T, args ...string) {
	var eg errgroup.Group
	for i := range r {
		i := i
		eg.Go(func() error {
			r[i].RunApp(t, args...)
			return nil
		})
	}
	_ = eg.Wait()
}

// MockConfigs creates all the rollerApps' configs.
func (r RollerApps) MockConfigs(store bool, wsURL string) error {
	var eg errgroup.Group
	for _, roller := range r {
		roller := roller
		eg.Go(func() error {
			return roller.MockConfig(store, wsURL)
		})
	}
	return eg.Wait()
}

// Free releases rollerApps.
func (r RollerApps) Free() {
	var wg sync.WaitGroup
	wg.Add(len(r))
	for i := range r {
		i := i
		go func() {
			r[i].Free()
			wg.Done()
		}()
	}
	wg.Wait()
}

// WaitExit wait rollerApps stopped.
func (r RollerApps) WaitExit() {
	var wg sync.WaitGroup
	wg.Add(len(r))
	for i := range r {
		i := i
		go func() {
			r[i].WaitExit()
			wg.Done()
		}()
	}
	wg.Wait()
}
