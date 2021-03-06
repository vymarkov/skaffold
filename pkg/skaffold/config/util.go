/*
Copyright 2019 The Skaffold Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package config

import (
	"fmt"
	"io/ioutil"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/imdario/mergo"
	"github.com/mitchellh/go-homedir"
	"github.com/sirupsen/logrus"
	"gopkg.in/yaml.v2"

	"github.com/GoogleContainerTools/skaffold/pkg/skaffold/constants"
	kubectx "github.com/GoogleContainerTools/skaffold/pkg/skaffold/kubernetes/context"
	"github.com/GoogleContainerTools/skaffold/pkg/skaffold/util"
)

const (
	defaultConfigDir  = ".skaffold"
	defaultConfigFile = "config"
	tenDays           = time.Hour * 24 * 10
	threeMonths       = time.Hour * 24 * 90
)

var (
	// config-file content
	configFile     *GlobalConfig
	configFileErr  error
	configFileOnce sync.Once

	// config for a kubeContext
	config     *ContextConfig
	configErr  error
	configOnce sync.Once

	ReadConfigFile             = readConfigFileCached
	GetConfigForCurrentKubectx = getConfigForCurrentKubectx
	current                    = time.Now
)

// readConfigFileCached reads the specified file and returns the contents
// parsed into a GlobalConfig struct.
// This function will always return the identical data from the first read.
func readConfigFileCached(filename string) (*GlobalConfig, error) {
	configFileOnce.Do(func() {
		filenameOrDefault, err := ResolveConfigFile(filename)
		if err != nil {
			configFileErr = err
			return
		}
		configFile, configFileErr = ReadConfigFileNoCache(filenameOrDefault)
	})
	return configFile, configFileErr
}

// ResolveConfigFile determines the default config location, if the configFile argument is empty.
func ResolveConfigFile(configFile string) (string, error) {
	if configFile == "" {
		home, err := homedir.Dir()
		if err != nil {
			return "", fmt.Errorf("retrieving home directory: %w", err)
		}
		configFile = filepath.Join(home, defaultConfigDir, defaultConfigFile)
	}
	return configFile, util.VerifyOrCreateFile(configFile)
}

// ReadConfigFileNoCache reads the given config yaml file and unmarshals the contents.
// Only visible for testing, use ReadConfigFile instead.
func ReadConfigFileNoCache(configFile string) (*GlobalConfig, error) {
	contents, err := ioutil.ReadFile(configFile)
	if err != nil {
		return nil, fmt.Errorf("reading global config: %w", err)
	}
	config := GlobalConfig{}
	if err := yaml.Unmarshal(contents, &config); err != nil {
		return nil, fmt.Errorf("unmarshalling global skaffold config: %w", err)
	}
	return &config, nil
}

// GetConfigForCurrentKubectx returns the specific config to be modified based on the kubeContext.
// Either returns the config corresponding to the provided or current context,
// or the global config.
func getConfigForCurrentKubectx(configFile string) (*ContextConfig, error) {
	configOnce.Do(func() {
		cfg, err := ReadConfigFile(configFile)
		if err != nil {
			configErr = err
			return
		}
		kubeconfig, err := kubectx.CurrentConfig()
		if err != nil {
			configErr = err
			return
		}
		config, configErr = getConfigForKubeContextWithGlobalDefaults(cfg, kubeconfig.CurrentContext)
	})

	return config, configErr
}

func getConfigForKubeContextWithGlobalDefaults(cfg *GlobalConfig, kubeContext string) (*ContextConfig, error) {
	if kubeContext == "" {
		if cfg.Global == nil {
			return &ContextConfig{}, nil
		}
		return cfg.Global, nil
	}

	var mergedConfig ContextConfig
	for _, contextCfg := range cfg.ContextConfigs {
		if contextCfg.Kubecontext == kubeContext {
			logrus.Debugf("found config for context %q", kubeContext)
			mergedConfig = *contextCfg
		}
	}
	// in case there was no config for this kubeContext in cfg.ContextConfigs
	mergedConfig.Kubecontext = kubeContext

	if cfg.Global != nil {
		// if values are unset for the current context, retrieve
		// the global config and use its values as a fallback.
		if err := mergo.Merge(&mergedConfig, cfg.Global, mergo.WithAppendSlice); err != nil {
			return nil, fmt.Errorf("merging context-specific and global config: %w", err)
		}
	}
	return &mergedConfig, nil
}

func GetDefaultRepo(configFile string, cliValue *string) (string, error) {
	// CLI flag takes precedence.
	if cliValue != nil {
		return *cliValue, nil
	}
	cfg, err := GetConfigForCurrentKubectx(configFile)
	if err != nil {
		return "", err
	}

	return cfg.DefaultRepo, nil
}

func GetLocalCluster(configFile string, minikubeProfile string) (bool, error) {
	if minikubeProfile != "" {
		return true, nil
	}
	cfg, err := GetConfigForCurrentKubectx(configFile)
	if err != nil {
		return false, err
	}
	// when set, the local-cluster config takes precedence
	if cfg.LocalCluster != nil {
		return *cfg.LocalCluster, nil
	}

	config, err := kubectx.CurrentConfig()
	if err != nil {
		return true, err
	}
	return isDefaultLocal(config.CurrentContext), nil
}

func GetInsecureRegistries(configFile string) ([]string, error) {
	cfg, err := GetConfigForCurrentKubectx(configFile)
	if err != nil {
		return nil, err
	}

	return cfg.InsecureRegistries, nil
}

func isDefaultLocal(kubeContext string) bool {
	if kubeContext == constants.DefaultMinikubeContext ||
		kubeContext == constants.DefaultDockerForDesktopContext ||
		kubeContext == constants.DefaultDockerDesktopContext {
		return true
	}

	isKind, _ := IsKindCluster(kubeContext)
	return isKind
}

// IsKindCluster checks that the given `kubeContext` is talking to `kind`.
// It also returns the name of the `kind` cluster.
func IsKindCluster(kubeContext string) (bool, string) {
	switch {
	// With kind version < 0.6.0, the k8s context
	// is `[CLUSTER NAME]@kind`.
	// For eg: `cluster@kind`
	// the default name is `kind@kind`
	case strings.HasSuffix(kubeContext, "@kind"):
		return true, strings.TrimSuffix(kubeContext, "@kind")

	// With kind version >= 0.6.0, the k8s context
	// is `kind-[CLUSTER NAME]`.
	// For eg: `kind-cluster`
	// the default name is `kind-kind`
	case strings.HasPrefix(kubeContext, "kind-"):
		return true, strings.TrimPrefix(kubeContext, "kind-")

	default:
		return false, ""
	}
}

func IsUpdateCheckEnabled(configfile string) bool {
	cfg, err := GetConfigForCurrentKubectx(configfile)
	if err != nil {
		return true
	}
	return cfg == nil || cfg.UpdateCheck == nil || *cfg.UpdateCheck
}

func ShouldDisplayPrompt(configfile string) bool {
	cfg, disabled := isSurveyPromptDisabled(configfile)
	return !disabled && !recentlyPromptedOrTaken(cfg)
}

func isSurveyPromptDisabled(configfile string) (*ContextConfig, bool) {
	cfg, err := GetConfigForCurrentKubectx(configfile)
	if err != nil {
		return nil, false
	}
	return cfg, cfg != nil && cfg.Survey != nil && *cfg.Survey.DisablePrompt
}

func recentlyPromptedOrTaken(cfg *ContextConfig) bool {
	if cfg == nil || cfg.Survey == nil {
		return false
	}
	return lessThan(cfg.Survey.LastTaken, threeMonths) || lessThan(cfg.Survey.LastPrompted, tenDays)
}

func lessThan(date string, duration time.Duration) bool {
	t, err := time.Parse(time.RFC3339, date)
	if err != nil {
		logrus.Debugf("could not parse data %s", date)
		return false
	}
	return current().Sub(t) < duration
}
