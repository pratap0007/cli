// Copyright © 2019 The Tekton Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package pipelinerun

import (
	"time"

	"github.com/tektoncd/cli/pkg/actions"
	"github.com/tektoncd/cli/pkg/cli"
	trh "github.com/tektoncd/cli/pkg/taskrun"
	v1 "github.com/tektoncd/pipeline/pkg/apis/pipeline/v1"
	"github.com/tektoncd/pipeline/pkg/client/clientset/versioned"
	informers "github.com/tektoncd/pipeline/pkg/client/informers/externalversions"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/cache"
)

// Tracker tracks the progress of a PipelineRun
type Tracker struct {
	Name         string
	Ns           string
	Tekton       versioned.Interface
	ongoingTasks map[string]bool
}

// NewTracker returns a new instance of Tracker
func NewTracker(name string, ns string, tekton versioned.Interface) *Tracker {
	return &Tracker{
		Name:         name,
		Ns:           ns,
		Tekton:       tekton,
		ongoingTasks: map[string]bool{},
	}
}

// Monitor to observe the progress of PipelineRun. It emits
// an event upon starting of a new Pipeline's Task.
// allowed containers the name of the Pipeline tasks, which used as filter
// limit the events to only those tasks
func (t *Tracker) Monitor(allowed []string, c *cli.Clients, pr *v1.PipelineRun) <-chan []trh.Run {
	factory := informers.NewSharedInformerFactoryWithOptions(
		t.Tekton,
		time.Second*10,
		informers.WithNamespace(t.Ns),
		informers.WithTweakListOptions(pipelinerunOpts(t.Name)))

	gvr, _ := actions.GetGroupVersionResource(
		schema.GroupVersionResource{Group: "tekton.dev", Resource: "pipelineruns"},
		t.Tekton.Discovery(),
	)

	genericInformer, _ := factory.ForResource(*gvr)
	informer := genericInformer.Informer()

	stopC := make(chan struct{})
	trC := make(chan []trh.Run)

	go func() {
		<-stopC
		close(trC)
	}()

	eventHandler := func(obj interface{}) {
		var pipelinerunConverted *v1.PipelineRun
		err := actions.GetV1(schema.GroupVersionResource{Group: "tekton.dev", Resource: "pipelineruns"}, c, pr.Name, t.Ns, metav1.GetOptions{}, &pipelinerunConverted)
		if err != nil {
			return
		}

		pr := pipelinerunConverted
		taskRunMap, trStatus, err := getPipelineRunStatus(pr, c, t.Ns)
		if err != nil {
			return
		}
		trC <- t.findNewTaskruns(pr, taskRunMap, trStatus, allowed)

		if hasCompleted(pr) {
			close(stopC) // should close trC
		}
	}

	informer.AddEventHandler(
		cache.ResourceEventHandlerFuncs{
			AddFunc:    eventHandler,
			UpdateFunc: func(_, newObj interface{}) { eventHandler(newObj) },
		},
	)

	factory.Start(stopC)
	factory.WaitForCacheSync(stopC)

	return trC
}

func pipelinerunOpts(name string) func(opts *metav1.ListOptions) {
	return func(opts *metav1.ListOptions) {
		opts.FieldSelector = fields.OneTermEqualSelector("metadata.name", name).String()
	}

}

// handles changes to pipelinerun and pushes the Run information to the
// channel if the task is new and is in the allowed list of tasks
// returns true if the pipelinerun has finished
func (t *Tracker) findNewTaskruns(pr *v1.PipelineRun, taskRunMap map[string]string, trStatues map[string]*v1.PipelineRunTaskRunStatus, allowed []string) []trh.Run {
	ret := []trh.Run{}
	for _, tr := range pr.Status.ChildReferences {
		retries := 0
		if pr.Status.PipelineSpec != nil {
			for _, pipelineTask := range pr.Status.PipelineSpec.Tasks {
				if tr.PipelineTaskName == pipelineTask.Name {
					retries = pipelineTask.Retries
				}
			}
		}
		run := trh.Run{Name: taskRunMap[tr.PipelineTaskName], Task: tr.PipelineTaskName, Retries: retries}

		if t.loggingInProgress(tr.PipelineTaskName) ||
			!trh.HasScheduled(trStatues[taskRunMap[tr.PipelineTaskName]]) ||
			trh.IsFiltered(run, allowed) {
			continue
		}

		t.ongoingTasks[tr.PipelineTaskName] = true
		ret = append(ret, run)
	}
	return ret
}

func hasCompleted(pr *v1.PipelineRun) bool {
	if len(pr.Status.Conditions) == 0 {
		return false
	}
	return pr.Status.Conditions[0].Status != corev1.ConditionUnknown
}

func (t *Tracker) loggingInProgress(tr string) bool {
	_, ok := t.ongoingTasks[tr]
	return ok
}

func getPipelineRunStatus(pr *v1.PipelineRun, c *cli.Clients, ns string) (map[string]string, map[string]*v1.PipelineRunTaskRunStatus, error) {
	// If the PipelineRun is nil, just return
	if pr == nil {
		return nil, nil, nil
	}

	// If there are no child references or either TaskRuns or Runs is non-zero, return the existing TaskRuns and Runs maps
	if len(pr.Status.ChildReferences) == 0 {
		return nil, nil, nil
	}

	trStatuses := make(map[string]*v1.PipelineRunTaskRunStatus)
	taskRunMap := make(map[string]string)

	for _, cr := range pr.Status.ChildReferences {
		switch cr.Kind {
		case "TaskRun":
			var tr *v1.TaskRun
			err := actions.GetV1(schema.GroupVersionResource{Group: "tekton.dev", Resource: "taskruns"}, c, cr.Name, ns, metav1.GetOptions{}, &tr)
			if err != nil {
				return nil, nil, err
			}

			// fmt.Println("===43-4", *tr)

			trStatuses[cr.Name] = &v1.PipelineRunTaskRunStatus{
				PipelineTaskName: cr.PipelineTaskName,
				Status:           &tr.Status,
				WhenExpressions:  cr.WhenExpressions,
			}

			taskRunMap[cr.PipelineTaskName] = tr.Name

		//TODO: Needs to handle Run, CustomRun later
		default:
			// Don't do anything for unknown types.
		}
	}

	return taskRunMap, trStatuses, nil
}
