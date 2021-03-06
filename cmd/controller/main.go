/*
Copyright 2018 The Knative Authors

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

package main

import (
	"flag"
	"log"
	"time"

	kubeinformers "k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/knative/build-pipeline/pkg/logging"

	"github.com/knative/build-pipeline/pkg/reconciler"
	"github.com/knative/build-pipeline/pkg/reconciler/v1alpha1/pipelinerun"
	"github.com/knative/build-pipeline/pkg/reconciler/v1alpha1/taskrun"
	"github.com/knative/build-pipeline/pkg/system"
	sharedclientset "github.com/knative/pkg/client/clientset/versioned"
	"github.com/knative/pkg/controller"

	clientset "github.com/knative/build-pipeline/pkg/client/clientset/versioned"
	pipelineinformers "github.com/knative/build-pipeline/pkg/client/informers/externalversions"
	buildclientset "github.com/knative/build/pkg/client/clientset/versioned"
	buildinformers "github.com/knative/build/pkg/client/informers/externalversions"
	"github.com/knative/pkg/configmap"
	"github.com/knative/pkg/signals"
)

const (
	threadsPerController = 2
)

var (
	masterURL  string
	kubeconfig string
)

func main() {
	flag.Parse()
	loggingConfigMap, err := configmap.Load("/etc/config-logging")
	if err != nil {
		log.Fatalf("Error loading logging configuration: %v", err)
	}
	loggingConfig, err := logging.NewConfigFromMap(loggingConfigMap)
	if err != nil {
		log.Fatalf("Error parsing logging configuration: %v", err)
	}
	logger, atomicLevel := logging.NewLoggerFromConfig(loggingConfig, logging.ControllerLogKey)
	defer logger.Sync()

	logger.Info("Starting the Pipeline Controller")

	// set up signals so we handle the first shutdown signal gracefully
	stopCh := signals.SetupSignalHandler()

	cfg, err := clientcmd.BuildConfigFromFlags(masterURL, kubeconfig)
	if err != nil {
		logger.Fatalf("Error building kubeconfig: %v", err)
	}

	kubeClient, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		logger.Fatalf("Error building kubernetes clientset: %v", err)
	}

	sharedClient, err := sharedclientset.NewForConfig(cfg)
	if err != nil {
		logger.Fatalf("Error building shared clientset: %v", err)
	}

	pipelineClient, err := clientset.NewForConfig(cfg)
	if err != nil {
		logger.Fatalf("Error building pipeline clientset: %v", err)
	}

	buildClient, err := buildclientset.NewForConfig(cfg)
	if err != nil {
		logger.Fatalf("Error building Build clientset: %v", err)
	}

	configMapWatcher := configmap.NewInformedWatcher(kubeClient, system.Namespace)

	opt := reconciler.Options{
		KubeClientSet:     kubeClient,
		BuildClientSet:    buildClient,
		SharedClientSet:   sharedClient,
		PipelineClientSet: pipelineClient,
		ResyncPeriod:      time.Second * 30,
		Logger:            logger,
	}

	kubeInformerFactory := kubeinformers.NewSharedInformerFactory(kubeClient, opt.ResyncPeriod)
	pipelineInformerFactory := pipelineinformers.NewSharedInformerFactory(pipelineClient, opt.ResyncPeriod)
	buildInformerFactory := buildinformers.NewSharedInformerFactory(buildClient, opt.ResyncPeriod)

	taskInformer := pipelineInformerFactory.Pipeline().V1alpha1().Tasks()
	taskRunInformer := pipelineInformerFactory.Pipeline().V1alpha1().TaskRuns()
	resourceInformer := pipelineInformerFactory.Pipeline().V1alpha1().PipelineResources()
	buildInformer := buildInformerFactory.Build().V1alpha1().Builds()

	pipelineInformer := pipelineInformerFactory.Pipeline().V1alpha1().Pipelines()
	pipelineRunInformer := pipelineInformerFactory.Pipeline().V1alpha1().PipelineRuns()
	pipelineParamsInformer := pipelineInformerFactory.Pipeline().V1alpha1().PipelineParamses()
	// Build all of our controllers, with the clients constructed above.
	controllers := []*controller.Impl{
		// Pipeline Controllers
		taskrun.NewController(opt,
			taskRunInformer,
			taskInformer,
			buildInformer,
			resourceInformer,
		),
		pipelinerun.NewController(opt,
			pipelineRunInformer,
			pipelineInformer,
			taskInformer,
			taskRunInformer,
			pipelineParamsInformer,
		),
	}

	// Watch the logging config map and dynamically update logging levels.
	configMapWatcher.Watch(logging.ConfigName, logging.UpdateLevelFromConfigMap(logger, atomicLevel, logging.ControllerLogKey))

	kubeInformerFactory.Start(stopCh)
	pipelineInformerFactory.Start(stopCh)
	buildInformerFactory.Start(stopCh)
	if err := configMapWatcher.Start(stopCh); err != nil {
		logger.Fatalf("failed to start configuration manager: %v", err)
	}

	// Wait for the caches to be synced before starting controllers.
	logger.Info("Waiting for informer caches to sync")
	for i, synced := range []cache.InformerSynced{
		taskInformer.Informer().HasSynced,
		taskRunInformer.Informer().HasSynced,
		buildInformer.Informer().HasSynced,
		resourceInformer.Informer().HasSynced,
	} {
		if ok := cache.WaitForCacheSync(stopCh, synced); !ok {
			logger.Fatalf("failed to wait for cache at index %v to sync", i)
		}
	}

	logger.Info("Starting controllers")
	// Start all of the controllers.
	for _, ctrlr := range controllers {
		go func(ctrlr *controller.Impl) {
			// We don't expect this to return until stop is called,
			// but if it does, propagate it back.
			if runErr := ctrlr.Run(threadsPerController, stopCh); runErr != nil {
				logger.Fatalf("Error running controller: %v", runErr)
			}
		}(ctrlr)
	}

	<-stopCh
}

func init() {
	flag.StringVar(&kubeconfig, "kubeconfig", "", "Path to a kubeconfig. Only required if out-of-cluster.")
	flag.StringVar(&masterURL, "master", "", "The address of the Kubernetes API server. Overrides any value in kubeconfig. Only required if out-of-cluster.")
}
