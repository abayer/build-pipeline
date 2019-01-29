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

package pipelinerun

import (
	"context"
	"fmt"
	"reflect"
	"time"

	"github.com/knative/build-pipeline/pkg/apis/pipeline"
	"github.com/knative/build-pipeline/pkg/apis/pipeline/v1alpha1"
	informers "github.com/knative/build-pipeline/pkg/client/informers/externalversions/pipeline/v1alpha1"
	listers "github.com/knative/build-pipeline/pkg/client/listers/pipeline/v1alpha1"
	"github.com/knative/build-pipeline/pkg/reconciler"
	"github.com/knative/build-pipeline/pkg/reconciler/v1alpha1/pipelinerun/resources"
	"github.com/knative/build-pipeline/pkg/reconciler/v1alpha1/taskrun"
	duckv1alpha1 "github.com/knative/pkg/apis/duck/v1alpha1"
	"github.com/knative/pkg/controller"
	"github.com/knative/pkg/tracker"
	"go.uber.org/zap"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
)

const (
	// ReasonCouldntGetPipeline indicates that the reason for the failure status is that the
	// associated Pipeline couldn't be retrieved
	ReasonCouldntGetPipeline = "CouldntGetPipeline"
	// ReasonInvalidBindings indicates that the reason for the failure status is that the
	// PipelineResources bound in the PipelineRun didn't match those declared in the Pipeline
	ReasonInvalidBindings = "InvalidPipelineResourceBindings"
	// ReasonCouldntGetTask indicates that the reason for the failure status is that the
	// associated Pipeline's Tasks couldn't all be retrieved
	ReasonCouldntGetTask = "CouldntGetTask"
	// ReasonCouldntGetResource indicates that the reason for the failure status is that the
	// associated PipelineRun's bound PipelineResources couldn't all be retrieved
	ReasonCouldntGetResource = "CouldntGetResource"
	// ReasonFailedValidation indicated that the reason for failure status is
	// that pipelinerun failed runtime validation
	ReasonFailedValidation = "PipelineValidationFailed"
	// pipelineRunAgentName defines logging agent name for PipelineRun Controller
	pipelineRunAgentName = "pipeline-controller"
	// pipelineRunControllerName defines name for PipelineRun Controller
	pipelineRunControllerName = "PipelineRun"

	// Event reasons
	eventReasonFailed    = "PipelineRunFailed"
	eventReasonSucceeded = "PipelineRunSucceeded"
)

// Reconciler implements controller.Reconciler for Configuration resources.
type Reconciler struct {
	*reconciler.Base

	// listers index properties about resources
	pipelineRunLister listers.PipelineRunLister
	pipelineLister    listers.PipelineLister
	taskRunLister     listers.TaskRunLister
	taskLister        listers.TaskLister
	clusterTaskLister listers.ClusterTaskLister
	resourceLister    listers.PipelineResourceLister
	tracker           tracker.Interface
}

// Check that our Reconciler implements controller.Reconciler
var _ controller.Reconciler = (*Reconciler)(nil)

// NewController creates a new Configuration controller
func NewController(
	opt reconciler.Options,
	pipelineRunInformer informers.PipelineRunInformer,
	pipelineInformer informers.PipelineInformer,
	taskInformer informers.TaskInformer,
	clusterTaskInformer informers.ClusterTaskInformer,
	taskRunInformer informers.TaskRunInformer,
	resourceInformer informers.PipelineResourceInformer,
) *controller.Impl {

	r := &Reconciler{
		Base:              reconciler.NewBase(opt, pipelineRunAgentName),
		pipelineRunLister: pipelineRunInformer.Lister(),
		pipelineLister:    pipelineInformer.Lister(),
		taskLister:        taskInformer.Lister(),
		clusterTaskLister: clusterTaskInformer.Lister(),
		taskRunLister:     taskRunInformer.Lister(),
		resourceLister:    resourceInformer.Lister(),
	}

	impl := controller.NewImpl(r, r.Logger, pipelineRunControllerName, reconciler.MustNewStatsReporter(pipelineRunControllerName, r.Logger))

	r.Logger.Info("Setting up event handlers")
	pipelineRunInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    impl.Enqueue,
		UpdateFunc: controller.PassNew(impl.Enqueue),
		DeleteFunc: impl.Enqueue,
	})

	r.tracker = tracker.New(impl.EnqueueKey, 30*time.Minute)
	taskRunInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		UpdateFunc: controller.PassNew(r.tracker.OnChanged),
	})
	return impl
}

// Reconcile compares the actual state with the desired, and attempts to
// converge the two. It then updates the Status block of the Pipeline Run
// resource with the current status of the resource.
func (c *Reconciler) Reconcile(ctx context.Context, key string) error {
	// Convert the namespace/name string into a distinct namespace and name
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		c.Logger.Errorf("invalid resource key: %s", key)
		return nil
	}

	// Get the Pipeline Run resource with this namespace/name
	original, err := c.pipelineRunLister.PipelineRuns(namespace).Get(name)
	if errors.IsNotFound(err) {
		// The resource no longer exists, in which case we stop processing.
		c.Logger.Errorf("pipeline run %q in work queue no longer exists", key)
		return nil
	} else if err != nil {
		return err
	}

	// Don't modify the informer's copy.
	pr := original.DeepCopy()
	pr.Status.InitializeConditions()

	if isDone(&pr.Status) {
		c.Recorder.Event(pr, corev1.EventTypeNormal, eventReasonSucceeded, "PipelineRun completed successfully.")
		return nil
	}

	if err := c.tracker.Track(pr.GetTaskRunRef(), pr); err != nil {
		c.Logger.Errorf("Failed to create tracker for TaskRuns for PipelineRun %s: %v", pr.Name, err)
		c.Recorder.Event(pr, corev1.EventTypeWarning, eventReasonFailed, "Failed to create tracker for TaskRuns for PipelineRun")
		return err
	}

	// Reconcile this copy of the task run and then write back any status
	// updates regardless of whether the reconciliation errored out.
	err = c.reconcile(ctx, pr)
	if equality.Semantic.DeepEqual(original.Status, pr.Status) {
		// If we didn't change anything then don't call updateStatus.
		// This is important because the copy we loaded from the informer's
		// cache may be stale and we don't want to overwrite a prior update
		// to status with this stale state.
	} else if _, err := c.updateStatus(pr); err != nil {
		c.Logger.Warn("Failed to update PipelineRun status", zap.Error(err))
		c.Recorder.Event(pr, corev1.EventTypeWarning, eventReasonFailed, "PipelineRun failed to update")
		return err
	}

	if err == nil {
		c.Recorder.Event(pr, corev1.EventTypeNormal, eventReasonSucceeded, "PipelineRun reconciled successfully")
	} else {
		c.Recorder.Event(pr, corev1.EventTypeWarning, eventReasonFailed, "PipelineRun failed to reconcile")
	}
	return err
}

func (c *Reconciler) reconcile(ctx context.Context, pr *v1alpha1.PipelineRun) error {
	p, err := c.pipelineLister.Pipelines(pr.Namespace).Get(pr.Spec.PipelineRef.Name)
	if err != nil {
		// This Run has failed, so we need to mark it as failed and stop reconciling it
		pr.Status.SetCondition(&duckv1alpha1.Condition{
			Type:   duckv1alpha1.ConditionSucceeded,
			Status: corev1.ConditionFalse,
			Reason: ReasonCouldntGetPipeline,
			Message: fmt.Sprintf("Pipeline %s can't be found:%s",
				fmt.Sprintf("%s/%s", pr.Namespace, pr.Spec.PipelineRef.Name), err),
		})
		return nil
	}

	providedResources, err := resources.GetResourcesFromBindings(p, pr)
	if err != nil {
		// This Run has failed, so we need to mark it as failed and stop reconciling it
		pr.Status.SetCondition(&duckv1alpha1.Condition{
			Type:   duckv1alpha1.ConditionSucceeded,
			Status: corev1.ConditionFalse,
			Reason: ReasonInvalidBindings,
			Message: fmt.Sprintf("PipelineRun %s doesn't bind Pipeline %s's PipelineResources correctly: %s",
				fmt.Sprintf("%s/%s", pr.Namespace, pr.Name), fmt.Sprintf("%s/%s", pr.Namespace, pr.Spec.PipelineRef.Name), err),
		})
		return nil
	}

	pipelineState, err := resources.ResolvePipelineRun(
		pr.Name,
		func(name string) (v1alpha1.TaskInterface, error) {
			return c.taskLister.Tasks(pr.Namespace).Get(name)
		},
		func(name string) (v1alpha1.TaskInterface, error) {
			return c.clusterTaskLister.Get(name)
		},
		c.resourceLister.PipelineResources(pr.Namespace).Get,
		p.Spec.Tasks, providedResources,
	)

	if err != nil {
		// This Run has failed, so we need to mark it as failed and stop reconciling it
		switch err := err.(type) {
		case *resources.TaskNotFoundError:
			pr.Status.SetCondition(&duckv1alpha1.Condition{
				Type:   duckv1alpha1.ConditionSucceeded,
				Status: corev1.ConditionFalse,
				Reason: ReasonCouldntGetTask,
				Message: fmt.Sprintf("Pipeline %s can't be Run; it contains Tasks that don't exist: %s",
					fmt.Sprintf("%s/%s", p.Namespace, p.Name), err),
			})
		case *resources.ResourceNotFoundError:
			pr.Status.SetCondition(&duckv1alpha1.Condition{
				Type:   duckv1alpha1.ConditionSucceeded,
				Status: corev1.ConditionFalse,
				Reason: ReasonCouldntGetResource,
				Message: fmt.Sprintf("PipelineRun %s can't be Run; it tries to bind Resources that don't exist: %s",
					fmt.Sprintf("%s/%s", p.Namespace, pr.Name), err),
			})
		default:
			pr.Status.SetCondition(&duckv1alpha1.Condition{
				Type:   duckv1alpha1.ConditionSucceeded,
				Status: corev1.ConditionFalse,
				Reason: ReasonFailedValidation,
				Message: fmt.Sprintf("PipelineRun %s can't be Run; couldn't resolve all references: %s",
					fmt.Sprintf("%s/%s", p.Namespace, pr.Name), err),
			})
		}
		return nil
	}

	if err := resources.ValidateFrom(pipelineState); err != nil {
		// This Run has failed, so we need to mark it as failed and stop reconciling it
		pr.Status.SetCondition(&duckv1alpha1.Condition{
			Type:   duckv1alpha1.ConditionSucceeded,
			Status: corev1.ConditionFalse,
			Reason: ReasonFailedValidation,
			Message: fmt.Sprintf("Pipeline %s can't be Run; it invalid input/output linkages: %s",
				fmt.Sprintf("%s/%s", p.Namespace, pr.Name), err),
		})
		return nil
	}

	for _, rprt := range pipelineState {
		err := taskrun.ValidateResolvedTaskResources(rprt.PipelineTask.Params, rprt.ResolvedTaskResources)
		if err != nil {
			c.Logger.Errorf("Failed to validate pipelinerun %q with error %v", pr.Name, err)
			pr.Status.SetCondition(&duckv1alpha1.Condition{
				Type:    duckv1alpha1.ConditionSucceeded,
				Status:  corev1.ConditionFalse,
				Reason:  ReasonFailedValidation,
				Message: err.Error(),
			})
			return nil
		}
	}
	err = resources.ResolveTaskRuns(c.taskRunLister.TaskRuns(pr.Namespace).Get, pipelineState)
	if err != nil {
		return fmt.Errorf("error getting TaskRuns for Pipeline %s: %s", p.Name, err)
	}

	// If the pipelinerun is cancelled, cancel tasks and update status
	if isCancelled(pr.Spec) {
		return cancelPipelineRun(pr, pipelineState, c.PipelineClientSet)
	}

	serviceAccount := pr.Spec.ServiceAccount
	rprt := resources.GetNextTask(pr.Name, pipelineState, c.Logger)

	if err := getOrCreatePVC(pr, c.KubeClientSet); err != nil {
		c.Logger.Infof("PipelineRun failed to create/get volume %s", pr.Name)
		return fmt.Errorf("Failed to create/get persistent volume claim %s for task %q: %v", pr.Name, err, pr.Name)
	}

	if rprt != nil {
		c.Logger.Infof("Creating a new TaskRun object %s", rprt.TaskRunName)
		rprt.TaskRun, err = c.createTaskRun(c.Logger, rprt, pr, serviceAccount, p.Spec.Timeout)
		if err != nil {
			c.Recorder.Eventf(pr, corev1.EventTypeWarning, "TaskRunCreationFailed", "Failed to create TaskRun %q: %v", rprt.TaskRunName, err)
			return fmt.Errorf("error creating TaskRun called %s for PipelineTask %s from PipelineRun %s: %s", rprt.TaskRunName, rprt.PipelineTask.Name, pr.Name, err)
		}
	}

	before := pr.Status.GetCondition(duckv1alpha1.ConditionSucceeded)
	after := resources.GetPipelineConditionStatus(pr.Name, pipelineState, c.Logger, pr.Status.StartTime, p.Spec.Timeout)

	pr.Status.SetCondition(after)

	reconciler.EmitEvent(c.Recorder, before, after, pr)

	updateTaskRunsStatus(pr, pipelineState)

	c.Logger.Infof("PipelineRun %s status is being set to %s", pr.Name, pr.Status.GetCondition(duckv1alpha1.ConditionSucceeded))
	return nil
}

func updateTaskRunsStatus(pr *v1alpha1.PipelineRun, pipelineState []*resources.ResolvedPipelineRunTask) {
	for _, rprt := range pipelineState {
		if rprt.TaskRun != nil {
			pr.Status.TaskRuns[rprt.TaskRun.Name] = rprt.TaskRun.Status
		}
	}
}

func (c *Reconciler) createTaskRun(logger *zap.SugaredLogger, rprt *resources.ResolvedPipelineRunTask, pr *v1alpha1.PipelineRun, sa string, pipelineTimeout *metav1.Duration) (*v1alpha1.TaskRun, error) {
	ts := rprt.ResolvedTaskResources.TaskSpec.DeepCopy()
	if pipelineTimeout != nil {
		pTimeoutTime := pr.Status.StartTime.Add(pipelineTimeout.Duration)
		if ts.Timeout == nil || time.Now().Add(ts.Timeout.Duration).After(pTimeoutTime) {
			// Just in case something goes awry and we're creating the TaskRun after it should have already timed out,
			// set a timeout of 0.
			taskRunTimeout := pTimeoutTime.Sub(time.Now())
			if taskRunTimeout < 0 {
				taskRunTimeout = 0
			}
			ts.Timeout = &metav1.Duration{Duration: taskRunTimeout}
		}
	}

	tr := &v1alpha1.TaskRun{
		ObjectMeta: metav1.ObjectMeta{
			Name:            rprt.TaskRunName,
			Namespace:       pr.Namespace,
			OwnerReferences: pr.GetOwnerReference(),
			Labels: map[string]string{
				pipeline.GroupName + pipeline.PipelineLabelKey:    pr.Spec.PipelineRef.Name,
				pipeline.GroupName + pipeline.PipelineRunLabelKey: pr.Name,
			},
		},
		Spec: v1alpha1.TaskRunSpec{
			TaskSpec: ts,
			Inputs: v1alpha1.TaskRunInputs{
				Params: rprt.PipelineTask.Params,
			},
			ServiceAccount: sa,
		}}

	resources.WrapSteps(&tr.Spec, rprt.PipelineTask, rprt.ResolvedTaskResources.Inputs, rprt.ResolvedTaskResources.Outputs)

	return c.PipelineClientSet.PipelineV1alpha1().TaskRuns(pr.Namespace).Create(tr)
}

func (c *Reconciler) updateStatus(pr *v1alpha1.PipelineRun) (*v1alpha1.PipelineRun, error) {
	newPr, err := c.pipelineRunLister.PipelineRuns(pr.Namespace).Get(pr.Name)
	if err != nil {
		return nil, fmt.Errorf("Error getting PipelineRun %s when updating status: %s", pr.Name, err)
	}
	if !reflect.DeepEqual(pr.Status, newPr.Status) {
		newPr.Status = pr.Status
		return c.PipelineClientSet.PipelineV1alpha1().PipelineRuns(pr.Namespace).Update(newPr)
	}
	return newPr, nil
}

func getOrCreatePVC(pr *v1alpha1.PipelineRun, c kubernetes.Interface) error {
	if _, err := c.CoreV1().PersistentVolumeClaims(pr.Namespace).Get(pr.GetPVCName(), metav1.GetOptions{}); err != nil {
		if errors.IsNotFound(err) {
			pvc := pr.GetPVC()
			if _, err := c.CoreV1().PersistentVolumeClaims(pr.Namespace).Create(pvc); err != nil {
				return fmt.Errorf("failed to claim Persistent Volume %q due to error: %s", pr.Name, err)
			}
			return nil
		}
		return fmt.Errorf("failed to get claim Persistent Volume %q due to error: %s", pr.Name, err)
	}
	return nil
}

// isDone returns true if the PipelineRun's status indicates the build is done.
func isDone(status *v1alpha1.PipelineRunStatus) bool {
	return !status.GetCondition(duckv1alpha1.ConditionSucceeded).IsUnknown()
}
