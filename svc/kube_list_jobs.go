package svc

import (
	"context"
	"time"

	"github.com/nerdalize/nerd/pkg/kubevisor"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
)

//JobDetailsPhase is a high level description of the underlying pod
type JobDetailsPhase string

var (
	// JobDetailsPhasePending means the pod has been accepted by the system, but one or more of the containers
	// has not been started. This includes time before being bound to a node, as well as time spent
	// pulling images onto the host.
	JobDetailsPhasePending JobDetailsPhase = "Pending"
	// JobDetailsPhaseRunning means the pod has been bound to a node and all of the containers have been started.
	// At least one container is still running or is in the process of being restarted.
	JobDetailsPhaseRunning JobDetailsPhase = "Running"
	// JobDetailsPhaseSucceeded means that all containers in the pod have voluntarily terminated
	// with a container exit code of 0, and the system is not going to restart any of these containers.
	JobDetailsPhaseSucceeded JobDetailsPhase = "Succeeded"
	// JobDetailsPhaseFailed means that all containers in the pod have terminated, and at least one container has
	// terminated in a failure (exited with a non-zero exit code or was stopped by the system).
	JobDetailsPhaseFailed JobDetailsPhase = "Failed"
	// JobDetailsPhaseUnknown means that for some reason the state of the pod could not be obtained, typically due
	// to an error in communicating with the host of the pod.
	JobDetailsPhaseUnknown JobDetailsPhase = "Unknown"
)

//JobDetails tells us more about the job by looking at underlying resources
type JobDetails struct {
	SeenAt         time.Time
	Phase          JobDetailsPhase
	Parallelism    int32  //job width, if 0 this means it was stopped
	WaitingReason  string //why the job -> pod -> container is waiting
	WaitingMessage string //explains why we're waiting
}

//ListJobItem is a job listing item
type ListJobItem struct {
	Name        string
	Image       string
	CreatedAt   time.Time
	DeletedAt   time.Time
	ActiveAt    time.Time
	CompletedAt time.Time
	FailedAt    time.Time

	Details JobDetails
}

//ListJobsInput is the input to ListJobs
type ListJobsInput struct{}

//ListJobsOutput is the output to ListJobs
type ListJobsOutput struct {
	Items []*ListJobItem
}

//ListJobs will create a job on kubernetes
func (k *Kube) ListJobs(ctx context.Context, in *ListJobsInput) (out *ListJobsOutput, err error) {
	if err = k.checkInput(ctx, in); err != nil {
		return nil, err
	}

	//Step 0: Get all the jobs under nerd-app=cli
	jobs := &jobs{}
	err = k.visor.ListResources(ctx, kubevisor.ResourceTypeJobs, jobs)
	if err != nil {
		return nil, err
	}

	//Step 1: Analyse job structure and formulate our output items
	out = &ListJobsOutput{}
	mapping := map[types.UID]*ListJobItem{}
	for _, job := range jobs.Items {
		if len(job.Spec.Template.Spec.Containers) != 1 {
			k.logs.Debugf("skipping job '%s' in namespace '%s' as it has not just 1 container", job.Name, job.Namespace)
			continue
		}

		c := job.Spec.Template.Spec.Containers[0]
		item := &ListJobItem{
			Name:      job.GetName(),
			Image:     c.Image,
			CreatedAt: job.CreationTimestamp.Local(),
		}

		if parr := job.Spec.Parallelism; parr != nil {
			item.Details.Parallelism = *parr
		}

		if dt := job.GetDeletionTimestamp(); dt != nil {
			item.DeletedAt = dt.Local() //mark as deleting
		}

		if job.Status.StartTime != nil {
			item.ActiveAt = job.Status.StartTime.Local()
		}

		for _, cond := range job.Status.Conditions {
			if cond.Status != corev1.ConditionTrue {
				continue
			}

			switch cond.Type {
			case batchv1.JobComplete:
				item.CompletedAt = cond.LastTransitionTime.Local()
			case batchv1.JobFailed:
				item.FailedAt = cond.LastTransitionTime.Local()
			}
		}

		mapping[job.UID] = item
		out.Items = append(out.Items, item)
	}

	//Step 2: Get all pods under nerd-app=cli
	pods := &pods{}
	err = k.visor.ListResources(ctx, kubevisor.ResourceTypePods, pods)
	if err != nil {
		return nil, err
	}

	//Step 3: Match pods to the jobs we got earlier and augment details with pod information
	for _, pod := range pods.Items {
		uid, ok := pod.Labels["controller-uid"]
		if !ok {
			continue //not part of a controller
		}

		jobItem, ok := mapping[types.UID(uid)]
		if !ok {
			continue //not part of any job
		}

		//technically we can have multiple pods per job (one terminating, unkown etc) so we pick the
		//one that is created most recently to base our details on
		if pod.CreationTimestamp.Local().After(jobItem.Details.SeenAt) {
			jobItem.Details.SeenAt = pod.CreationTimestamp.Local()
		}

		//the pod phase allows us to distinguish between Pending and Running
		switch pod.Status.Phase {
		case corev1.PodPending:
			jobItem.Details.Phase = JobDetailsPhasePending
		case corev1.PodRunning:
			jobItem.Details.Phase = JobDetailsPhaseRunning
		case corev1.PodFailed:
			jobItem.Details.Phase = JobDetailsPhaseFailed
		case corev1.PodSucceeded:
			jobItem.Details.Phase = JobDetailsPhaseSucceeded
		default:
			jobItem.Details.Phase = JobDetailsPhaseUnknown
		}

		//container conditions allow us to capture ErrImageNotFound
		for _, cstatus := range pod.Status.ContainerStatuses {
			if cstatus.Name != "main" { //we only care about the main container
				continue
			}

			//waiting reasons give us ErrImagePull/Backoff
			if cstatus.State.Waiting != nil {
				jobItem.Details.WaitingReason = cstatus.State.Waiting.Reason
				jobItem.Details.WaitingMessage = cstatus.State.Waiting.Message
			}
		}
	}

	return out, nil
}

//jobs implements the list transformer interface to allow the kubevisor the manage names for us
type jobs struct{ *batchv1.JobList }

func (jobs *jobs) Transform(fn func(in kubevisor.ManagedNames) (out kubevisor.ManagedNames)) {
	for i, j1 := range jobs.JobList.Items {
		jobs.Items[i] = *(fn(&j1).(*batchv1.Job))
	}
}

//pods implements the list transformer interface to allow the kubevisor the manage names for us
type pods struct{ *corev1.PodList }

func (pods *pods) Transform(fn func(in kubevisor.ManagedNames) (out kubevisor.ManagedNames)) {
	for i, j1 := range pods.PodList.Items {
		pods.Items[i] = *(fn(&j1).(*corev1.Pod))
	}
}
