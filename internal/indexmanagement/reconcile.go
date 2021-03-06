package indexmanagement

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"

	"github.com/ViaQ/logerr/kverrors"
	batchv1 "k8s.io/api/batch/v1"
	batch "k8s.io/api/batch/v1beta1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/ViaQ/logerr/log"
	apis "github.com/openshift/elasticsearch-operator/apis/logging/v1"
	"github.com/openshift/elasticsearch-operator/internal/constants"
	"github.com/openshift/elasticsearch-operator/internal/types/k8s"
	"github.com/openshift/elasticsearch-operator/internal/utils"
	"github.com/openshift/elasticsearch-operator/internal/utils/comparators"
)

const (
	indexManagementConfigmap = "indexmanagement-scripts"
	defaultShardSize         = int32(40)
)

var (
	defaultCPURequest      = resource.MustParse("100m")
	defaultMemoryRequest   = resource.MustParse("32Mi")
	jobHistoryLimitFailed  = utils.GetInt32(1)
	jobHistoryLimitSuccess = utils.GetInt32(1)

	millisPerSecond = uint64(1000)
	millisPerMinute = uint64(60 * millisPerSecond)
	millisPerHour   = uint64(millisPerMinute * 60)
	millisPerDay    = uint64(millisPerHour * 24)
	millisPerWeek   = uint64(millisPerDay * 7)

	// fullExecMode octal 0777
	fullExecMode int32 = 0o777

	imLabels = map[string]string{
		"provider":      "openshift",
		"component":     "indexManagement",
		"logging-infra": "indexManagement",
	}
)

type rolloverConditions struct {
	MaxAge  string `json:"max_age,omitempty"`
	MaxDocs int32  `json:"max_docs,omitempty"`
	MaxSize string `json:"max_size,omitempty"`
}

func RemoveCronJobsForMappings(apiclient client.Client, cluster *apis.Elasticsearch, mappings []apis.IndexManagementPolicyMappingSpec, policies apis.PolicyMap) error {
	expected := sets.NewString()
	for _, mapping := range mappings {
		policy := policies[mapping.PolicyRef]
		if policy.Phases.Hot != nil {
			expected.Insert(fmt.Sprintf("%s-rollover-%s", cluster.Name, mapping.Name))
		}
		if policy.Phases.Delete != nil {
			expected.Insert(fmt.Sprintf("%s-delete-%s", cluster.Name, mapping.Name))
		}
	}

	cronList := &batch.CronJobList{}
	listOpts := []client.ListOption{
		client.InNamespace(cluster.Namespace),
		client.MatchingLabels(imLabels),
	}
	if err := apiclient.List(context.TODO(), cronList, listOpts...); err != nil {
		return kverrors.Wrap(err, "failed to list cron jobs",
			"namespace", cluster.Namespace,
			"labels", imLabels,
		)
	}
	existing := sets.NewString()
	for _, cron := range cronList.Items {
		existing.Insert(cron.Name)
	}
	difference := existing.Difference(expected)
	for _, name := range difference.List() {
		cronjob := &batch.CronJob{
			TypeMeta: metav1.TypeMeta{
				Kind:       "CronJob",
				APIVersion: batch.SchemeGroupVersion.String(),
			},
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: cluster.Namespace,
			},
		}
		err := apiclient.Delete(context.TODO(), cronjob)
		if err != nil && !apierrors.IsNotFound(err) {
			log.Error(err, "failed to remove cronjob", "namespace", cluster.Namespace, "name", name)
		}
	}
	return nil
}

func ReconcileCurationConfigmap(apiclient client.Client, cluster *apis.Elasticsearch) error {
	data := scriptMap
	desired := k8s.NewConfigMap(indexManagementConfigmap, cluster.Namespace, imLabels, data)
	cluster.AddOwnerRefTo(desired)

	errCtx := kverrors.NewContext("configmap", desired.Name,
		"cluster", cluster.Name,
		"namespace", cluster.Namespace,
	)

	err := apiclient.Create(context.TODO(), desired)
	if err == nil {
		return nil
	}
	if !apierrors.IsAlreadyExists(err) {
		return errCtx.Wrap(err, "failed to create cluster configmap")
	}
	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		current := &corev1.ConfigMap{}
		retryError := apiclient.Get(context.TODO(), types.NamespacedName{Name: desired.Name, Namespace: desired.Namespace}, current)
		if retryError != nil {
			return retryError
		}
		if !reflect.DeepEqual(desired.Data, current.Data) {
			current.Data = desired.Data
			return apiclient.Update(context.TODO(), current)
		}
		return nil
	})
	return errCtx.Wrap(err, "failed to update configmap")
}

func ReconcileRolloverCronjob(apiclient client.Client, cluster *apis.Elasticsearch, policy apis.IndexManagementPolicySpec, mapping apis.IndexManagementPolicyMappingSpec, primaryShards int32) error {
	if policy.Phases.Hot == nil {
		log.Info("Skipping rollover cronjob for policymapping; hot phase not defined", "policymapping", mapping.Name)
		return nil
	}
	schedule, err := crontabScheduleFor(policy.PollInterval)
	if err != nil {
		return kverrors.Wrap(err, "failed to reconcile rollover cronjob", "policymapping", mapping.Name)
	}
	conditions := calculateConditions(policy, primaryShards)
	name := fmt.Sprintf("%s-rollover-%s", cluster.Name, mapping.Name)
	payload, err := json.Marshal(map[string]rolloverConditions{"conditions": conditions})
	if err != nil {
		return kverrors.Wrap(err, "failed to serialize the rollover conditions to JSON")
	}
	envvars := []corev1.EnvVar{
		{Name: "PAYLOAD", Value: base64.StdEncoding.EncodeToString(payload)},
		{Name: "POLICY_MAPPING", Value: mapping.Name},
	}
	fnContainerHandler := func(container *corev1.Container) {
		container.Command = []string{"bash"}
		container.Args = []string{
			"-c",
			"/tmp/scripts/rollover",
		}
	}
	desired := newCronJob(cluster.Name, constants.PackagedElasticsearchImage(), cluster.Namespace, name, schedule, cluster.Spec.Spec.NodeSelector, cluster.Spec.Spec.Tolerations, envvars, fnContainerHandler)

	cluster.AddOwnerRefTo(desired)
	return reconcileCronJob(apiclient, cluster, desired, areCronJobsSame)
}

func ReconcileCurationCronjob(apiclient client.Client, cluster *apis.Elasticsearch, policy apis.IndexManagementPolicySpec, mapping apis.IndexManagementPolicyMappingSpec, primaryShards int32) error {
	if policy.Phases.Delete == nil {
		log.Info("Skipping curation cronjob for policymapping; delete phase not defined", "policymapping", mapping.Name)
		return nil
	}
	schedule, err := crontabScheduleFor(policy.PollInterval)
	if err != nil {
		return err
	}
	minAgeMillis, err := calculateMillisForTimeUnit(policy.Phases.Delete.MinAge)
	if err != nil {
		return err
	}
	name := fmt.Sprintf("%s-delete-%s", cluster.Name, mapping.Name)
	envvars := []corev1.EnvVar{
		{Name: "POLICY_MAPPING", Value: mapping.Name},
		{Name: "MIN_AGE", Value: strconv.FormatUint(minAgeMillis, 10)},
	}
	fnContainerHandler := func(container *corev1.Container) {
		container.Command = []string{"bash"}
		container.Args = []string{
			"-c",
			"/tmp/scripts/delete",
		}
	}
	desired := newCronJob(cluster.Name, constants.PackagedElasticsearchImage(), cluster.Namespace, name, schedule, cluster.Spec.Spec.NodeSelector, cluster.Spec.Spec.Tolerations, envvars, fnContainerHandler)

	cluster.AddOwnerRefTo(desired)
	return reconcileCronJob(apiclient, cluster, desired, areCronJobsSame)
}

func reconcileCronJob(apiclient client.Client, cluster *apis.Elasticsearch, desired *batch.CronJob, fnAreCronJobsSame func(lhs, rhs *batch.CronJob) bool) error {
	err := apiclient.Create(context.TODO(), desired)
	if err == nil {
		return nil
	}
	if !apierrors.IsAlreadyExists(err) {
		return kverrors.Wrap(err, "failed to create cronjob for cluster",
			"namespace", cluster.Namespace,
			"cluster", cluster.Name)
	}
	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		current := &batch.CronJob{}
		retryError := apiclient.Get(context.TODO(), types.NamespacedName{Name: desired.Name, Namespace: desired.Namespace}, current)
		if retryError != nil {
			return retryError
		}
		if !fnAreCronJobsSame(current, desired) {
			current.Spec = desired.Spec
			return apiclient.Update(context.TODO(), current)
		}
		return nil
	})
	return kverrors.Wrap(err, "failed to update cronjob for cluster",
		"namespace", desired.Namespace,
		"cluster", desired.Name)
}

func areCronJobsSame(lhs, rhs *batch.CronJob) bool {
	if len(lhs.Spec.JobTemplate.Spec.Template.Spec.Containers) != len(lhs.Spec.JobTemplate.Spec.Template.Spec.Containers) {
		return false
	}
	if !comparators.AreStringMapsSame(lhs.Spec.JobTemplate.Spec.Template.Spec.NodeSelector, rhs.Spec.JobTemplate.Spec.Template.Spec.NodeSelector) {
		return false
	}

	if !comparators.AreTolerationsSame(lhs.Spec.JobTemplate.Spec.Template.Spec.Tolerations, rhs.Spec.JobTemplate.Spec.Template.Spec.Tolerations) {
		return false
	}
	if lhs.Spec.Schedule != rhs.Spec.Schedule {
		lhs.Spec.Schedule = rhs.Spec.Schedule
		return false
	}
	if lhs.Spec.Suspend != nil && rhs.Spec.Suspend != nil && *lhs.Spec.Suspend != *rhs.Spec.Suspend {
		return false
	}

	for i, container := range lhs.Spec.JobTemplate.Spec.Template.Spec.Containers {
		other := rhs.Spec.JobTemplate.Spec.Template.Spec.Containers[i]
		if container.Name != other.Name {
			return false
		}
		if container.Image != other.Image {
			return false
		}

		if !reflect.DeepEqual(container.Command, other.Command) {
			return false
		}
		if !reflect.DeepEqual(container.Args, other.Args) {
			return false
		}

		if !comparators.AreResourceRequementsSame(container.Resources, other.Resources) {
			return false
		}

		if !comparators.EnvValueEqual(container.Env, other.Env) {
			return false
		}

	}
	return true
}

func newCronJob(clusterName, image, namespace, name, schedule string, nodeSelector map[string]string, tolerations []corev1.Toleration, envvars []corev1.EnvVar, fnContainerHander func(*corev1.Container)) *batch.CronJob {
	container := corev1.Container{
		Name:            "indexmanagement",
		Image:           image,
		ImagePullPolicy: corev1.PullIfNotPresent,
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceMemory: defaultMemoryRequest,
				corev1.ResourceCPU:    defaultCPURequest,
			},
		},
		Env: []corev1.EnvVar{
			{Name: "ES_SERVICE", Value: fmt.Sprintf("https://%s:9200", clusterName)},
		},
	}
	container.Env = append(container.Env, envvars...)
	fnContainerHander(&container)

	container.VolumeMounts = []corev1.VolumeMount{
		{Name: "certs", ReadOnly: true, MountPath: "/etc/indexmanagement/keys"},
		{Name: "scripts", ReadOnly: false, MountPath: "/tmp/scripts"},
	}
	podSpec := corev1.PodSpec{
		ServiceAccountName: clusterName,
		Containers:         []corev1.Container{container},
		Volumes: []corev1.Volume{
			{Name: "certs", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: clusterName}}},
			{Name: "scripts", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: indexManagementConfigmap}, DefaultMode: &fullExecMode}}},
		},
		NodeSelector:                  utils.EnsureLinuxNodeSelector(nodeSelector),
		Tolerations:                   tolerations,
		RestartPolicy:                 corev1.RestartPolicyNever,
		TerminationGracePeriodSeconds: utils.GetInt64(300),
	}

	cronJob := &batch.CronJob{
		TypeMeta: metav1.TypeMeta{
			Kind:       "CronJob",
			APIVersion: batch.SchemeGroupVersion.String(),
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    imLabels,
		},
		Spec: batch.CronJobSpec{
			ConcurrencyPolicy:          batch.ForbidConcurrent,
			SuccessfulJobsHistoryLimit: jobHistoryLimitSuccess,
			FailedJobsHistoryLimit:     jobHistoryLimitFailed,
			Schedule:                   schedule,
			JobTemplate: batch.JobTemplateSpec{
				Spec: batchv1.JobSpec{
					BackoffLimit: utils.GetInt32(0),
					Parallelism:  utils.GetInt32(1),
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Name:      name,
							Namespace: namespace,
							Labels:    imLabels,
						},
						Spec: podSpec,
					},
				},
			},
		},
	}

	return cronJob
}
