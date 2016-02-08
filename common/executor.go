package common

import (
	log "github.com/Sirupsen/logrus"
)

type ExecutorData interface{}

type Executor interface {
	Prepare(globalConfig *Config, config *RunnerConfig, build *Build) error
	Start() error
	Wait() error
	Finish(err error)
	Cleanup()
	Abort()
}

type ExecutorProvider interface {
	CanCreate() bool
	Create() Executor
	Acquire(config *RunnerConfig) (ExecutorData, error)
	Release(config *RunnerConfig, data ExecutorData) error
	GetFeatures(features *FeaturesInfo)
}

var executors map[string]ExecutorProvider

func RegisterExecutor(executor string, provider ExecutorProvider) {
	log.Debugln("Registering", executor, "executor...")

	if executors == nil {
		executors = make(map[string]ExecutorProvider)
	}
	if _, ok := executors[executor]; ok {
		panic("Executor already exist: " + executor)
	}
	executors[executor] = provider
}

func GetExecutor(executor string) ExecutorProvider {
	if executors == nil {
		return nil
	}

	provider, _ := executors[executor]
	return provider
}

func GetExecutors() []string {
	names := []string{}
	if executors != nil {
		for name := range executors {
			names = append(names, name)
		}
	}
	return names
}

func NewExecutor(executor string) Executor {
	provider := GetExecutor(executor)
	if provider != nil {
		return provider.Create()
	}

	return nil
}
