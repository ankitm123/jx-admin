package joblog

import (
	"context"
	"errors"
	"fmt"
	"github.com/jenkins-x-plugins/jx-admin/pkg/common"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/jenkins-x-plugins/jx-admin/pkg/bootjobs"
	"github.com/jenkins-x/jx-helpers/v3/pkg/cobras/helper"
	"github.com/jenkins-x/jx-helpers/v3/pkg/cobras/templates"
	"github.com/jenkins-x/jx-helpers/v3/pkg/input"
	"github.com/jenkins-x/jx-helpers/v3/pkg/input/inputfactory"
	"github.com/jenkins-x/jx-helpers/v3/pkg/kube"
	"github.com/jenkins-x/jx-helpers/v3/pkg/kube/jobs"
	"github.com/jenkins-x/jx-helpers/v3/pkg/kube/podlogs"
	"github.com/jenkins-x/jx-helpers/v3/pkg/kube/pods"
	"github.com/jenkins-x/jx-helpers/v3/pkg/options"
	"github.com/jenkins-x/jx-helpers/v3/pkg/stringhelpers"
	"github.com/jenkins-x/jx-helpers/v3/pkg/termcolor"
	"github.com/jenkins-x/jx-kube-client/v3/pkg/kubeclient"
	logger "github.com/jenkins-x/jx-logging/v3/pkg/log"

	"github.com/spf13/cobra"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// Options contains the command line arguments for this command
type Options struct {
	options.BaseOptions

	Namespace           string
	JobSelector         string
	GitOperatorSelector string
	ContainerName       string
	CommitSHA           string
	Duration            time.Duration
	PollPeriod          time.Duration
	NoTail              bool
	ShaMode             bool
	WaitMode            bool
	ErrOut              io.Writer
	Out                 io.Writer
	KubeClient          kubernetes.Interface
	Input               input.Interface
	timeEnd             time.Time
	podStatusMap        map[string]string
}

var (
	info = termcolor.ColorInfo

	cmdLong = templates.LongDesc(`
		Views the boot Job logs in the cluster

`)

	cmdExample = templates.Examples(`
* views the current boot logs
` + bashExample("log") + `
`)
)

// bashExample returns markdown for a bash script expression
func bashExample(cli string) string {
	return fmt.Sprintf("\n```bash \n%s %s\n```\n", common.BinaryName, cli)
}

// NewCmdJobLog creates the new command
func NewCmdJobLog() (*cobra.Command, *Options) {
	o := &Options{}
	command := &cobra.Command{
		Use:     "log",
		Short:   "views the boot Job logs in the cluster",
		Aliases: []string{"logs"},
		Long:    cmdLong,
		Example: cmdExample,
		Run: func(command *cobra.Command, args []string) {
			err := o.Run()
			helper.CheckErr(err)
		},
	}
	command.Flags().StringVarP(&o.Namespace, "namespace", "n", "", "the namespace where the boot jobs run. If not specified it will look in: jx-git-operator and jx")
	command.Flags().StringVarP(&o.JobSelector, "selector", "s", "app=jx-boot", "the selector of the boot Job pods")
	command.Flags().StringVarP(&o.GitOperatorSelector, "git-operator-selector", "g", "app=jx-git-operator", "the selector of the git operator pod")
	command.Flags().StringVarP(&o.ContainerName, "container", "c", "job", "the name of the container in the boot Job to log")
	command.Flags().StringVarP(&o.CommitSHA, "commit-sha", "", "", "the git commit SHA of the git repository to query the boot Job for")
	command.Flags().BoolVarP(&o.WaitMode, "wait", "w", false, "wait for the next active Job to start")
	command.Flags().BoolVarP(&o.ShaMode, "sha-mode", "", false, "if --commit-sha is not specified then default the git commit SHA from $ and fail if it could not be found")
	command.Flags().DurationVarP(&o.Duration, "duration", "d", time.Minute*30, "how long to wait for a Job to be active and a Pod to be ready")
	command.Flags().DurationVarP(&o.PollPeriod, "poll", "", time.Second*1, "duration between polls for an active Job or Pod")

	o.BaseOptions.AddBaseFlags(command)

	return command, o
}

func (o *Options) Run() error {
	err := o.Validate()
	if err != nil {
		return err
	}

	client := o.KubeClient
	selector := o.JobSelector
	containerName := o.ContainerName

	ns, err := bootjobs.FindGitOperatorNamespace(client, o.Namespace)
	if err != nil {
		return fmt.Errorf("failed to find the git operator namespace: %w", err)
	}

	sortedJobs, err := bootjobs.GetSortedJobs(client, ns, selector, o.CommitSHA)
	if err != nil {
		return fmt.Errorf("failed to get jobs: %w", err)
	}

	if !o.WaitMode && len(sortedJobs) <= 1 {
		if len(sortedJobs) == 0 {
			o.WaitMode = true
		} else {
			j := sortedJobs[0]
			if j.Status.Active > 0 {
				o.WaitMode = true
			}
		}
	}
	if o.WaitMode {
		err = o.waitForGitOperator(client, ns, selector)
		if err != nil {
			return fmt.Errorf("failed to wait for git operator: %w", err)
		}
		return o.waitForActiveJob(client, ns, selector, info, containerName)
	}
	return o.pickJobToLog(client, ns, selector, sortedJobs)
}

func (o *Options) waitForGitOperator(client kubernetes.Interface, ns, selector string) error {
	o.timeEnd = time.Now().Add(o.Duration)
	logger.Logger().Infof("waiting for the Git Operator to be ready in namespace %s...", info(ns))

	goPod, err := pods.WaitForPodSelectorToBeReady(client, ns, o.GitOperatorSelector, o.Duration)
	if err != nil {
		return fmt.Errorf("failed waiting for the git operator pod to be ready in namespace %s with selector %s: %w", ns, o.GitOperatorSelector, err)
	}
	if goPod == nil {
		logger.Logger().Infof(`Could not find the git operator. 

Are you sure you have installed the git operator?

See: https://jenkins-x.io/docs/v3/guides/operator/

`)
		return fmt.Errorf("no git operator pod to be ready in namespace %s with selector %s: %w", ns, o.GitOperatorSelector, err)
	}
	logger.Logger().Infof("the Git Operator is running in pod %s\n\n", info(goPod.Name))

	if o.CommitSHA != "" {
		logger.Logger().Infof("waiting for boot Job pod with selector %s in namespace %s for commit SHA %s...", info(selector), info(ns), info(o.CommitSHA))

	} else {
		logger.Logger().Infof("waiting for boot Job pod with selector %s in namespace %s...", info(selector), info(ns))
	}
	return nil
}

func (o *Options) waitForActiveJob(client kubernetes.Interface, ns, selector string, info func(a ...interface{}) string, containerName string) error {
	job, err := o.waitForLatestJob(client, ns, selector)
	if err != nil {
		return fmt.Errorf("failed to wait for active Job in namespace %s with selector %v: %w", ns, selector, err)
	}

	logger.Logger().Infof("waiting for Job %s to complete...", info(job.Name))

	return o.viewActiveJobLog(client, ns, selector, containerName, job)
}

func (o *Options) viewActiveJobLog(client kubernetes.Interface, ns, selector, containerName string, job *batchv1.Job) error {
	var foundPods []string
	for {
		complete, pod, err := o.waitForJobCompleteOrPodRunning(client, ns, selector, job.Name)
		if err != nil {
			return err
		}
		if complete {
			return nil
		}
		if pod == nil {
			return fmt.Errorf("No pod found for namespace %s with selector %v", ns, selector)
		}

		if time.Now().After(o.timeEnd) {
			return fmt.Errorf("timed out after waiting for duration %s", o.Duration.String())
		}

		// lets verify the container name
		err = verifyContainerName(pod, containerName)
		if err != nil {
			return err
		}
		podName := pod.Name
		if stringhelpers.StringArrayIndex(foundPods, podName) < 0 {
			foundPods = append(foundPods, podName)
		}
		logger.Logger().Infof("\ntailing boot Job pod %s\n\n", info(podName))

		err = podlogs.TailLogs(ns, podName, containerName, o.ErrOut, o.Out)
		if err != nil {
			logger.Logger().Warnf("failed to tail log: %s", err.Error())
		}
		pod, err = client.CoreV1().Pods(ns).Get(context.TODO(), podName, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("failed to get pod %s in namespace %s: %w", podName, ns, err)
		}
		if pods.IsPodCompleted(pod) {
			if pods.IsPodSucceeded(pod) {
				logger.Logger().Infof("boot Job pod %s has %s", info(podName), info("Succeeded"))
			} else {
				logger.Logger().Infof("boot Job pod %s is %s", info(podName), termcolor.ColorError(string(pod.Status.Phase)))
			}
		} else if pod.DeletionTimestamp != nil {
			logger.Logger().Infof("boot Job pod %s is %s", info(podName), termcolor.ColorWarning("Terminating"))
		}
	}
}

func (o *Options) viewJobLog(client kubernetes.Interface, ns, selector, containerName string, job *batchv1.Job) error {
	jobName := job.Name
	opts := metav1.ListOptions{
		LabelSelector: "job-name=" + jobName,
	}
	podList, err := client.CoreV1().Pods(ns).List(context.TODO(), opts)
	if err != nil && apierrors.IsNotFound(err) {
		err = nil
	}
	if err != nil {
		return fmt.Errorf("failed to list pods in namespace %s with selector %s: %w", ns, selector, err)
	}

	pos := podList.Items
	var lastPod *corev1.Pod
	// Sort pods in creation time order
	sort.Slice(pos, func(i, j int) bool {
		return pos[i].CreationTimestamp.Before(&pos[j].CreationTimestamp)
	})
	for i := range pos {
		pod := &pos[i]

		// lets verify the container name
		err = verifyContainerName(pod, containerName)
		if err != nil {
			return err
		}

		// wait for a pod to be running, ready or completed
		condition := func(pod *v1.Pod) bool {
			return pods.IsPodReady(pod) || pods.IsPodCompleted(pod) || pod.Status.Phase == corev1.PodRunning
		}
		err = pods.WaitforPodNameCondition(client, ns, pod.Name, o.Duration, condition)
		if err != nil {
			return fmt.Errorf("failed to wait for pod %s to be running: %w", pod.Name, err)
		}
		podName := pod.Name
		logger.Logger().Infof("\ntailing boot Job pod %s created %s\n\n", info(podName), info(pod.CreationTimestamp))

		err = podlogs.TailLogs(ns, podName, containerName, o.ErrOut, o.Out)
		if err != nil {
			logger.Logger().Warnf("failed to tail log: %s", err.Error())
		}
		pod, err = client.CoreV1().Pods(ns).Get(context.TODO(), podName, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("failed to get pod %s in namespace %s: %w", podName, ns, err)
		}
		if pods.IsPodCompleted(pod) {
			if pods.IsPodSucceeded(pod) {
				logger.Logger().Infof("boot Job pod %s has %s", info(podName), info("Succeeded"))
			} else {
				logger.Logger().Infof("boot Job pod %s has %s", info(podName), termcolor.ColorError(string(pod.Status.Phase)))
			}
		} else if pod.DeletionTimestamp != nil {
			logger.Logger().Infof("boot Job pod %s is %s", info(podName), termcolor.ColorWarning("Terminating"))
		}
		lastPod = pod
	}
	// If job is active return error if latest pod has failed
	job, err = client.BatchV1().Jobs(ns).Get(context.TODO(), jobName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to get boot Job %s: %w", jobName, err)
	}
	if !jobs.IsJobFinished(job) && lastPod != nil {
		if !pods.IsPodSucceeded(lastPod) {
			return fmt.Errorf("boot Job pod %s has %s", lastPod.Name, string(lastPod.Status.Phase))
		}
	} else if !jobs.IsJobSucceeded(job) {
		if len(job.Status.Conditions) > 0 {
			// If job is finished return error if job has failed
			return errors.New(job.Status.Conditions[0].Message)
		}
	}

	return nil
}

// Validate verifies the settings are correct and we can lazy create any required resources
func (o *Options) Validate() error {
	if o.NoTail {
		return nil
	}
	if o.ErrOut == nil {
		o.ErrOut = os.Stderr
	}
	if o.Out == nil {
		o.Out = os.Stdout
	}
	if o.ShaMode && o.CommitSHA == "" {
		o.CommitSHA = os.Getenv("PULL_BASE_SHA")
		if o.ShaMode && o.CommitSHA == "" {
			return fmt.Errorf("you have specified --sha-mode but no $PULL_BASE_SHA is defined or --commit-sha option supplied")
		}
	}

	var err error
	o.KubeClient, err = kube.LazyCreateKubeClientWithMandatory(o.KubeClient, true)
	if err != nil {
		return fmt.Errorf("failed to create kubernetes client: %w", err)
	}
	if o.Namespace == "" {
		o.Namespace, err = kubeclient.CurrentNamespace()
		if err != nil {
			return fmt.Errorf("failed to detect current namespace. Try supply --namespace: %w", err)
		}
	}
	if o.Input == nil {
		o.Input = inputfactory.NewInput(&o.BaseOptions)
	}
	return nil
}

func (o *Options) waitForLatestJob(client kubernetes.Interface, ns, selector string) (*batchv1.Job, error) {
	for {
		job, err := o.getLatestJob(client, ns, selector)
		if err != nil {
			return nil, fmt.Errorf("failed to : %w", err)
		}

		if job != nil {
			if o.CommitSHA != "" || !jobs.IsJobFinished(job) {
				return job, nil
			}
		}

		if time.Now().After(o.timeEnd) {
			return nil, fmt.Errorf("timed out after waiting for duration %s", o.Duration.String())
		}
		time.Sleep(o.PollPeriod)
	}
}

func (o *Options) waitForJobCompleteOrPodRunning(client kubernetes.Interface, ns, selector, jobName string) (bool, *corev1.Pod, error) {
	if o.podStatusMap == nil {
		o.podStatusMap = map[string]string{}
	}

	for {
		complete, job, err := o.checkIfJobComplete(client, ns, jobName)
		if err != nil {
			return false, nil, fmt.Errorf("failed to check for Job %s complete: %w", jobName, err)
		}
		if complete {
			if job != nil && !jobs.IsJobSucceeded(job) {
				return true, nil, fmt.Errorf("job %s failed", jobName)
			}
			return true, nil, nil
		}

		pod, err := pods.GetReadyPodForSelector(client, ns, selector)
		if err != nil {
			return false, pod, fmt.Errorf("failed to query ready pod in namespace %s with selector %s: %w", ns, selector, err)
		}
		if pod != nil {
			status := pods.PodStatus(pod)
			if o.podStatusMap[pod.Name] != status && !pods.IsPodCompleted(pod) && pod.DeletionTimestamp == nil {
				logger.Logger().Infof("pod %s has status %s", termcolor.ColorInfo(pod.Name), termcolor.ColorInfo(status))
				o.podStatusMap[pod.Name] = status
			}
			if pod.Status.Phase == v1.PodRunning || pods.IsPodReady(pod) {
				return false, pod, nil
			}
		}

		if time.Now().After(o.timeEnd) {
			return false, nil, fmt.Errorf("timed out after waiting for duration %s", o.Duration.String())
		}
		time.Sleep(o.PollPeriod)
	}
}

func (o *Options) getLatestJob(client kubernetes.Interface, ns, selector string) (*batchv1.Job, error) {
	jobList, err := client.BatchV1().Jobs(ns).List(context.TODO(), metav1.ListOptions{
		LabelSelector: selector,
	})
	if err != nil && !apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("failed to list jobList in namespace %s selector %s: %w", ns, selector, err)
	}
	if len(jobList.Items) == 0 {
		return nil, nil
	}

	if o.CommitSHA != "" {
		for i := 0; i < len(jobList.Items); i++ {
			job := &jobList.Items[i]
			labels := job.Labels
			if labels != nil {
				if o.CommitSHA == labels[bootjobs.LabelCommitSHA] {
					return job, nil
				}
			}
		}
		return nil, nil
	}

	// lets find the newest job...
	latest := jobList.Items[0]
	for i := 1; i < len(jobList.Items); i++ {
		job := jobList.Items[i]
		if job.CreationTimestamp.After(latest.CreationTimestamp.Time) {
			latest = job
		}
	}
	return &latest, nil
}

func (o *Options) checkIfJobComplete(client kubernetes.Interface, ns, name string) (bool, *batchv1.Job, error) {
	job, err := client.BatchV1().Jobs(ns).Get(context.TODO(), name, metav1.GetOptions{})
	if job == nil || err != nil {
		return false, nil, fmt.Errorf("failed to list jobList in namespace %s name %s: %w", ns, name, err)
	}
	if jobs.IsJobFinished(job) {
		if jobs.IsJobSucceeded(job) {
			logger.Logger().Infof("boot Job %s has %s", info(job.Name), info("Succeeded"))
			return true, job, nil
		}
		logger.Logger().Infof("boot Job %s has %s", info(job.Name), termcolor.ColorError("Failed"))
		return true, job, nil
	}
	logger.Logger().Debugf("boot Job %s is not completed yet", info(job.Name))
	return false, job, nil
}

func (o *Options) pickJobToLog(client kubernetes.Interface, ns, selector string, jobs []batchv1.Job) error {
	var names []string
	m := map[string]*batchv1.Job{}
	for i := range jobs {
		j := &jobs[i]
		name := toJobName(j, len(jobs)-i)
		m[name] = j
		names = append(names, name)
	}

	name, err := o.Input.PickNameWithDefault(names, "select the Job to view:", "", "select which boot Job you wish to log")
	if err != nil {
		return fmt.Errorf("failed to pick a boot job name: %w", err)
	}
	if name == "" {
		return fmt.Errorf("no boot Jobs to view. Try add --active to wait for the next boot job")
	}
	job := m[name]
	if job == nil {
		return fmt.Errorf("cannot find Job %s", name)
	}
	return o.viewJobLog(client, ns, selector, o.ContainerName, job)
}

func toJobName(j *batchv1.Job, number int) string {
	status := JobStatus(j)
	d := time.Since(j.CreationTimestamp.Time).Round(time.Minute)
	return fmt.Sprintf("#%d started %s %s", number, d.String(), status)
}

func JobStatus(j *batchv1.Job) string {
	if jobs.IsJobSucceeded(j) {
		return "Succeeded"
	}
	if jobs.IsJobFinished(j) {
		return "Failed"
	}
	if j.Status.Active > 0 {
		return "Running"
	}
	if j.Spec.Suspend != nil && *j.Spec.Suspend {
		return "Suspended"
	}

	return "Pending"
}

func verifyContainerName(pod *corev1.Pod, name string) error {
	var names []string
	for i := range pod.Spec.Containers {
		if pod.Spec.Containers[i].Name == name {
			return nil
		}
		names = append(names, pod.Spec.Containers[i].Name)
	}
	sort.Strings(names)
	return fmt.Errorf("invalid container name %s for pod %s. Available names: %s", name, pod.Name, strings.Join(names, ", "))
}
