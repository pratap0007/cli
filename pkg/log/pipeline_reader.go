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

package log

import (
	"fmt"
	"sync"
	"time"

	"github.com/tektoncd/cli/pkg/actions"
	"github.com/tektoncd/cli/pkg/pipelinerun"
	trh "github.com/tektoncd/cli/pkg/taskrun"
	v1 "github.com/tektoncd/pipeline/pkg/apis/pipeline/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	duckv1 "knative.dev/pkg/apis/duck/v1"
)

func (r *Reader) readPipelineLog() (<-chan Log, <-chan error, error) {

	gvr := schema.GroupVersionResource{Group: "tekton.dev", Resource: "pipelineruns"}
	var pr *v1.PipelineRun
	err := actions.GetV1(gvr, r.clients, r.run, r.ns, metav1.GetOptions{}, &pr)
	if err != nil {
		return nil, nil, err
	}

	if !pr.IsDone() && r.follow {
		return r.readLivePipelineLogs(pr)
	}
	return r.readAvailablePipelineLogs(pr)
}

func (r *Reader) readLivePipelineLogs(pr *v1.PipelineRun) (<-chan Log, <-chan error, error) {
	logC := make(chan Log)
	errC := make(chan error)

	go func() {
		defer close(logC)
		defer close(errC)

		prTracker := pipelinerun.NewTracker(pr.Name, r.ns, r.clients.Tekton)

		// if task is not passed as flag
		if len(r.tasks) == 0 {
			// to collect tasks
			for _, pt := range pr.Status.ChildReferences {
				r.tasks = append(r.tasks, pt.PipelineTaskName)
			}

			// to collect the finally task
			var pl *v1.Pipeline
			err := actions.GetV1(schema.GroupVersionResource{Group: "tekton.dev", Resource: "pipelines"}, r.clients, pr.Spec.PipelineRef.Name, r.ns, metav1.GetOptions{}, &pl)
			if err != nil {
				return
			}
			for _, pt := range pl.Spec.Finally {
				r.tasks = append(r.tasks, pt.Name)
			}

		}

		trC := prTracker.Monitor(r.tasks, r.clients, pr)
		wg := sync.WaitGroup{}
		taskIndex := 0

		for trs := range trC {
			wg.Add(len(trs))
			for _, run := range trs {
				taskIndex++
				// NOTE: passing tr, taskIdx to avoid data race
				go func(tr trh.Run, taskNum int) {
					defer wg.Done()

					// clone the object to keep task number and name separately
					c := r.clone()
					c.setUpTask(int(taskNum), tr)
					c.pipeLogs(logC, errC)

				}(run, taskIndex)
			}
		}

		wg.Wait()

		if !empty(pr.Status) && pr.Status.Conditions[0].Status == corev1.ConditionFalse {
			errC <- fmt.Errorf(pr.Status.Conditions[0].Message)
		}
	}()

	return logC, errC, nil
}

func (r *Reader) readAvailablePipelineLogs(pr *v1.PipelineRun) (<-chan Log, <-chan error, error) {
	if err := r.waitUntilAvailable(); err != nil {
		return nil, nil, err
	}

	ordered, err := r.getOrderedTasks(pr)
	if err != nil {
		return nil, nil, err
	}

	taskRuns := trh.Filter(ordered, r.tasks)

	logC := make(chan Log)
	errC := make(chan error)

	go func() {
		defer close(logC)
		defer close(errC)

		// clone the object to keep task number and name separately
		c := r.clone()
		for i, tr := range taskRuns {
			c.setUpTask(i+1, tr)
			c.pipeLogs(logC, errC)
		}

		if !empty(pr.Status) && pr.Status.Conditions[0].Status == corev1.ConditionFalse {
			errC <- fmt.Errorf(pr.Status.Conditions[0].Message)
		}
	}()

	return logC, errC, nil
}

// reading of logs should wait till the status of run is unknown
// only if run status is unknown, open a watch channel on run
// and keep checking the status until it changes to true|false
// or the reach timeout
func (r *Reader) waitUntilAvailable() error {
	var first = true
	opts := metav1.ListOptions{
		FieldSelector: fields.OneTermEqualSelector("metadata.name", r.run).String(),
	}

	gvr := schema.GroupVersionResource{Group: "tekton.dev", Resource: "pipelineruns"}
	watchRun, err := pipelinerun.Watch(r.clients, opts, r.ns)
	if err != nil {
		return err
	}
	for {
		// var newPR *v1.PipelineRun
		// err := actions.GetV1(gvr, r.clients, r.run, r.ns, metav1.GetOptions{}, &newPR)

		var run *v1.PipelineRun
		err := actions.GetV1(gvr, r.clients, r.run, r.ns, metav1.GetOptions{}, &run)
		if err != nil {
			return err
		}
		if empty(run.Status) {
			return nil
		}

		if run.Status.Conditions[0].Status != corev1.ConditionUnknown {
			return nil
		}
		// run = newPR
		select {
		case event := <-watchRun.ResultChan():
			run, err := cast2pipelinerun(event.Object)
			if err != nil {
				return err
			}
			if run.IsDone() == true {
				watchRun.Stop()
				return nil
			}
			if first {
				first = false
				fmt.Fprintln(r.stream.Out, "Pipeline still running ...")
			}
		case <-time.After(r.activityTimeout):
			watchRun.Stop()
			if isPipelineRunRunning(run.Status.Conditions) {
				fmt.Fprintln(r.stream.Out, "PipelineRun is still running:", run.Status.Conditions[0].Message)
				return nil
			}
			if err = hasPipelineRunFailed(run.Status.Conditions); err != nil {
				return fmt.Errorf("PipelineRun %s has failed: %s", run.Name, err.Error())
			}
			return fmt.Errorf("PipelineRun has not started yet")
		}
	}
}

func (r *Reader) pipeLogs(logC chan<- Log, errC chan<- error) {
	tlogC, terrC, err := r.readTaskLog()
	if err != nil {
		errC <- err
		return
	}

	for tlogC != nil || terrC != nil {
		select {
		case l, ok := <-tlogC:
			if !ok {
				tlogC = nil
				continue
			}
			logC <- Log{Task: l.Task, Step: l.Step, Log: l.Log}

		case e, ok := <-terrC:
			if !ok {
				terrC = nil
				continue
			}
			errC <- fmt.Errorf("failed to get logs for task %s : %s", r.task, e)
		}
	}
}

func (r *Reader) setUpTask(taskNumber int, tr trh.Run) {
	r.setNumber(taskNumber)
	r.setRun(tr.Name)
	r.setTask(tr.Task)
	r.setRetries(tr.Retries)
}

// getOrderedTasks get Tasks in order from Spec.PipelineRef or Spec.PipelineSpec
// and return trh.Run after converted taskruns into trh.Run.
func (r *Reader) getOrderedTasks(pr *v1.PipelineRun) ([]trh.Run, error) {
	var tasks []v1.PipelineTask
	var pl *v1.Pipeline

	switch {
	case pr.Spec.PipelineRef != nil:

		err := actions.GetV1(schema.GroupVersionResource{Group: "tekton.dev", Resource: "pipelines"}, r.clients, pr.Spec.PipelineRef.Name, r.ns, metav1.GetOptions{}, &pl)
		if err != nil {
			return nil, err
		}
		tasks = pl.Spec.Tasks
		tasks = append(tasks, pl.Spec.Finally...)
	case pr.Spec.PipelineSpec != nil:
		tasks = pr.Spec.PipelineSpec.Tasks
		tasks = append(tasks, pr.Spec.PipelineSpec.Finally...)
	default:
		return nil, fmt.Errorf("pipelinerun %s did not provide PipelineRef or PipelineSpec", pr.Name)
	}

	taskrunMapStatus := make(map[string]v1.PipelineRunTaskRunStatus)
	pipelineTaskTaskRunMap := make(map[string]string)

	for _, taskrun := range pr.Status.ChildReferences {

		var tr *v1.TaskRun
		err := actions.GetV1(schema.GroupVersionResource{Group: "tekton.dev", Resource: "taskruns"}, r.clients, taskrun.Name, r.ns, metav1.GetOptions{}, &tr)
		if err != nil {
			return nil, err
		}

		pipelineTaskTaskRunMap[taskrun.PipelineTaskName] = tr.Name
		taskrunMapStatus[taskrun.Name] = v1.PipelineRunTaskRunStatus{Status: &tr.Status}
	}

	// Sort taskruns, to display the taskrun logs as per pipeline tasks order
	return trh.SortTasksBySpecOrder(tasks, pipelineTaskTaskRunMap, taskrunMapStatus), nil
}

func empty(status v1.PipelineRunStatus) bool {
	if status.Conditions == nil {
		return true
	}
	return len(status.Conditions) == 0
}

func hasPipelineRunFailed(prConditions duckv1.Conditions) error {
	if len(prConditions) != 0 && prConditions[0].Status == corev1.ConditionFalse {
		return fmt.Errorf("pipelinerun has failed: %s", prConditions[0].Message)
	}
	return nil
}

func isPipelineRunRunning(prConditions duckv1.Conditions) bool {
	if len(prConditions) != 0 && prConditions[0].Status == corev1.ConditionUnknown {
		return true
	}
	return false
}

func cast2pipelinerun(obj runtime.Object) (*v1.PipelineRun, error) {
	var run *v1.PipelineRun
	unstruct, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
	if err != nil {
		return nil, err
	}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(unstruct, &run); err != nil {
		return nil, err
	}
	return run, nil
}
