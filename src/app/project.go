package app

import (
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/f1bonacc1/process-compose/src/pclog"
	"github.com/joho/godotenv"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"gopkg.in/yaml.v2"
)

var PROJ *Project

func (p *Project) Run() {
	p.initProcessStates()
	p.runningProcesses = make(map[string]*Process)
	runOrder := []ProcessConfig{}
	p.WithProcesses([]string{}, func(process ProcessConfig) error {
		runOrder = append(runOrder, process)
		return nil
	})
	var nameOrder []string
	for _, v := range runOrder {
		nameOrder = append(nameOrder, v.Name)
	}
	p.logger = pclog.NewNilLogger()
	if isStringDefined(p.LogLocation) {
		p.logger = pclog.NewLogger(p.LogLocation)
		defer p.logger.Close()
	}
	log.Debug().Msgf("Spinning up %d processes. Order: %q", len(runOrder), nameOrder)
	for _, proc := range runOrder {
		p.runProcess(proc)
	}
	p.wg.Wait()
}

func (p *Project) runProcess(proc ProcessConfig) {
	procLogger := p.logger
	if isStringDefined(proc.LogLocation) {
		procLogger = pclog.NewLogger(proc.LogLocation)
	}
	process := NewProcess(p.Environment, procLogger, proc, p.GetProcessState(proc.Name), 1)
	p.addRunningProcess(process)
	p.wg.Add(1)
	go func() {
		defer p.removeRunningProcess(process.GetName())
		defer p.wg.Done()
		if err := p.waitIfNeeded(process.procConf); err != nil {
			log.Error().Msgf("Error: %s", err.Error())
			log.Error().Msgf("Error: process %s won't run", process.GetName())
			process.WontRun()
		} else {
			process.Run()
		}
	}()
}

func (p *Project) waitIfNeeded(process ProcessConfig) error {
	for k := range process.DependsOn {
		if runningProc := p.getRunningProcess(k); runningProc != nil {

			switch process.DependsOn[k].Condition {
			case ProcessConditionCompleted:
				runningProc.WaitForCompletion(process.Name)
			case ProcessConditionCompletedSuccessfully:
				log.Info().Msgf("%s is waiting for %s to complete successfully", process.Name, k)
				exitCode := runningProc.WaitForCompletion(process.Name)
				if exitCode != 0 {
					return fmt.Errorf("process %s depended on %s to complete successfully, but it exited with status %d",
						process.Name, k, exitCode)
				}
			}
		}
	}
	return nil
}

func (p *Project) initProcessStates() {
	p.processStates = make(map[string]*ProcessState)
	for key, proc := range p.Processes {
		p.processStates[key] = &ProcessState{
			Name:     key,
			Status:   ProcessStatePending,
			Restarts: 0,
			ExitCode: 0,
		}
		if proc.Disabled {
			p.processStates[key].Status = ProcessStateDisabled
		}
	}
}

func (p *Project) GetProcessState(name string) *ProcessState {
	if procState, ok := p.processStates[name]; ok {
		return procState
	}
	log.Error().Msgf("Error: process %s doesn't exist", name)
	return nil
}

func (p *Project) addRunningProcess(process *Process) {
	p.mapMutex.Lock()
	p.runningProcesses[process.GetName()] = process
	p.mapMutex.Unlock()
}

func (p *Project) getRunningProcess(name string) *Process {
	p.mapMutex.Lock()
	defer p.mapMutex.Unlock()
	if runningProc, ok := p.runningProcesses[name]; ok {
		return runningProc
	}
	return nil
}

func (p *Project) removeRunningProcess(name string) {
	p.mapMutex.Lock()
	delete(p.runningProcesses, name)
	p.mapMutex.Unlock()
}

func (p *Project) StartProcess(name string) error {
	proc := p.getRunningProcess(name)
	if proc != nil {
		log.Error().Msgf("Process %s is already running", name)
		return fmt.Errorf("process %s is already running", name)
	}
	if proc, ok := p.Processes[name]; ok {
		proc.Name = name
		p.runProcess(proc)
	} else {
		return fmt.Errorf("no such process: %s", name)
	}

	return nil
}

func (p *Project) StopProcess(name string) error {
	proc := p.getRunningProcess(name)
	if proc == nil {
		log.Error().Msgf("Process %s is not running", name)
		return fmt.Errorf("process %s is not running", name)
	}
	proc.stop()
	return nil
}

func (p *Project) getProcesses(names ...string) ([]ProcessConfig, error) {
	processes := []ProcessConfig{}
	if len(names) == 0 {
		for name, proc := range p.Processes {
			if proc.Disabled {
				continue
			}
			proc.Name = name
			processes = append(processes, proc)
		}
		return processes, nil
	}
	for _, name := range names {
		if proc, ok := p.Processes[name]; ok {
			if proc.Disabled {
				continue
			}
			proc.Name = name
			processes = append(processes, proc)
		} else {
			return processes, fmt.Errorf("no such process: %s", name)
		}
	}

	return processes, nil
}

type ProcessFunc func(process ProcessConfig) error

// WithProcesses run ProcesseFunc on each Process and dependencies in dependency order
func (p *Project) WithProcesses(names []string, fn ProcessFunc) error {
	return p.withProcesses(names, fn, map[string]bool{})
}

func (p *Project) withProcesses(names []string, fn ProcessFunc, done map[string]bool) error {
	processes, err := p.getProcesses(names...)
	if err != nil {
		return err
	}
	for _, process := range processes {
		if done[process.Name] {
			continue
		}
		done[process.Name] = true

		dependencies := process.GetDependencies()
		if len(dependencies) > 0 {
			err := p.withProcesses(dependencies, fn, done)
			if err != nil {
				return err
			}
		}
		if err := fn(process); err != nil {
			return err
		}
	}
	return nil
}

func (p *Project) GetDependenciesOrderNames() ([]string, error) {

	order := []string{}
	err := p.WithProcesses([]string{}, func(process ProcessConfig) error {
		order = append(order, process.Name)
		return nil
	})
	return order, err
}

func (p *Project) GetLexicographicProcessNames() ([]string, error) {

	names := []string{}
	for name := range p.Processes {
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

func CreateProject(inputFile string) *Project {
	yamlFile, err := ioutil.ReadFile(inputFile)

	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			log.Error().Msgf("File %s doesn't exist", inputFile)
		}
		log.Fatal().Msg(err.Error())
	}

	// .env is optional we don't care if it errors
	godotenv.Load()

	yamlFile = []byte(os.ExpandEnv(string(yamlFile)))

	var project Project
	err = yaml.Unmarshal(yamlFile, &project)
	if err != nil {
		log.Fatal().Msg(err.Error())
	}
	if project.LogLevel != "" {
		lvl, err := zerolog.ParseLevel(project.LogLevel)
		if err != nil {
			log.Error().Msgf("Unknown log level %s defaulting to %s",
				project.LogLevel, zerolog.GlobalLevel().String())
		} else {
			zerolog.SetGlobalLevel(lvl)
		}

	}
	PROJ = &project
	return &project
}

func findFiles(names []string, pwd string) []string {
	candidates := []string{}
	for _, n := range names {
		f := filepath.Join(pwd, n)
		if _, err := os.Stat(f); err == nil {
			candidates = append(candidates, f)
		}
	}
	return candidates
}

// DefaultFileNames defines the Compose file names for auto-discovery (in order of preference)
var DefaultFileNames = []string{"compose.yml", "compose.yaml", "process-compose.yml", "process-compose.yaml"}

func AutoDiscoverComposeFile(pwd string) (string, error) {
	candidates := findFiles(DefaultFileNames, pwd)
	if len(candidates) > 0 {
		winner := candidates[0]
		if len(candidates) > 1 {
			log.Warn().Msgf("Found multiple config files with supported names: %s", strings.Join(candidates, ", "))
			log.Warn().Msgf("Using %s", winner)
		}
		return winner, nil
	}
	return "", fmt.Errorf("no config files found in %s", pwd)
}
