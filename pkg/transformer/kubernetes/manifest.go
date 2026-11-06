/*
Copyright 2024 The Kubernetes Authors All rights reserved.

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

package kubernetes

import (
	"fmt"
	"sort"
	"strings"

	deployapi "github.com/openshift/api/apps/v1"
	log "github.com/sirupsen/logrus"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/kubernetes/kompose/pkg/kobject"
	"github.com/kubernetes/kompose/pkg/loader/compose"
	"github.com/kubernetes/kompose/pkg/manifest"
	"github.com/kubernetes/kompose/pkg/transformer"
)

// Redaction categories used in manifest sources. They state which values
// were intentionally omitted; the values themselves are never stored.
const (
	redactedEnvironment = "environment values"
	redactedSecretData  = "secret data"
)

// buildManifestContext holds precomputed name -> service indexes used while
// resolving the provenance of each generated object.
type buildManifestContext struct {
	services map[string]kobject.ServiceConfig
	// containerName -> normalized service name
	containers map[string]string
	// env configmap name -> service name
	envConfigMaps map[string]string
	// compose config configmap name -> service names (a config may be shared)
	fileConfigMaps map[string][]string
	// persistent volume claim name -> service names
	claims map[string][]string
	// secret name -> services mounting it
	secretConsumers map[string][]string
	// network name -> services connected to it
	networks map[string][]string
	// group name (kompose.service.group) -> member service names
	groups map[string][]string
}

func newBuildManifestContext(ko kobject.KomposeObject) *buildManifestContext {
	ctx := &buildManifestContext{
		services:        ko.ServiceConfigs,
		containers:      map[string]string{},
		envConfigMaps:   map[string]string{},
		fileConfigMaps:  map[string][]string{},
		claims:          map[string][]string{},
		secretConsumers: map[string][]string{},
		networks:        map[string][]string{},
		groups:          map[string][]string{},
	}
	for name, svc := range ko.ServiceConfigs {
		ctx.containers[name] = name
		ctx.containers[GetContainerName(svc)] = name

		for _, envFile := range svc.EnvFile {
			ctx.envConfigMaps[FormatEnvName(envFile, name)] = name
		}

		for _, cfg := range svc.Configs {
			cmName := FormatFileName(cfg.Source)
			ctx.fileConfigMaps[cmName] = append(ctx.fileConfigMaps[cmName], name)
		}

		for _, vol := range svc.Volumes {
			for _, candidate := range []string{vol.PVCName, vol.VolumeName} {
				if candidate == "" {
					continue
				}
				ctx.claims[FormatResourceName(candidate)] = appendUnique(ctx.claims[FormatResourceName(candidate)], name)
			}
		}

		for _, secret := range svc.Secrets {
			secretName := FormatResourceName(secret.Source)
			ctx.secretConsumers[secretName] = appendUnique(ctx.secretConsumers[secretName], name)
		}

		for _, net := range svc.Network {
			ctx.networks[net] = appendUnique(ctx.networks[net], name)
		}

		if group := svc.Labels[compose.LabelServiceGroup]; group != "" {
			ctx.groups[group] = appendUnique(ctx.groups[group], name)
		}
	}
	return ctx
}

func appendUnique(values []string, value string) []string {
	for _, v := range values {
		if v == value {
			return values
		}
	}
	return append(values, value)
}

// BuildManifest creates the traceability manifest from the final list of
// converted objects. It must be called after Transform; it only reads the
// objects and never modifies them.
func (k *Kubernetes) BuildManifest(objects []runtime.Object, tracker *manifest.Tracker, ko kobject.KomposeObject, opt kobject.ConvertOptions, docs manifest.DocumentIndex) *manifest.Manifest {
	ctx := newBuildManifestContext(ko)

	var records []manifest.ObjectRecord
	var warnings []string

	for _, obj := range objects {
		record := k.buildManifestRecord(obj, tracker, ko, opt, docs, ctx)
		for _, warning := range record.Warnings {
			log.Warnf("Manifest: %s (object %s/%s)", warning, record.Object.Kind, record.Object.Name)
			warnings = append(warnings, warning)
		}
		records = append(records, record)
	}

	warnings = append(warnings, docs.Warnings...)
	return manifest.New(opt.Provider, opt.InputFiles, records, warnings, opt.ManifestPageSize)
}

func (k *Kubernetes) buildManifestRecord(obj runtime.Object, tracker *manifest.Tracker, ko kobject.KomposeObject, opt kobject.ConvertOptions, docs manifest.DocumentIndex, ctx *buildManifestContext) manifest.ObjectRecord {
	ref, ok := manifest.RefOf(obj)
	if !ok {
		return manifest.ObjectRecord{
			Object: manifest.ObjectRef{Kind: fmt.Sprintf("%T", obj)},
			Warnings: []string{
				"object does not expose Kubernetes metadata, no provenance could be recorded",
			},
		}
	}
	record := manifest.ObjectRecord{Object: ref}

	serviceNames := resolveObjectServices(obj, ref, tracker, ctx)
	for _, name := range serviceNames {
		svc, ok := ko.ServiceConfigs[name]
		if !ok {
			continue
		}
		record.Sources = append(record.Sources, buildServiceSource(svc, ref.Kind, obj, opt, docs))
	}

	// Object-kind specific sources (root level secrets, options, defaults).
	record.Sources = append(record.Sources, extraSources(obj, ref, ko)...)

	// Warn when a workload mounts a Secret that has no file backed
	// declaration in the Compose project: the generated pod points at a
	// source kompose did not create.
	if pod := podTemplateSpec(obj); pod != nil {
		if warning := missingSecretWarning(pod, ko); warning != "" {
			record.Warnings = append(record.Warnings, warning)
		}
	}

	record.Sources = manifest.MergeSources(record.Sources...)

	if len(record.Sources) == 0 {
		record.Sources = []manifest.Source{{
			Type: manifest.SourceUnknown,
			Name: "unresolved",
			Item: manifest.Key(ref),
		}}
		record.Warnings = append(record.Warnings, fmt.Sprintf(
			"no originating Compose field or conversion option could be determined for %s %q", ref.Kind, ref.Name))
	}

	record.Stages = resolveStages(tracker, ref, record.Sources)
	return record
}

// resolveObjectServices returns the normalized names of the services that
// contribute to an object. Several signals are combined so grouped
// workloads, network_mode container sharing and generated helpers (env
// ConfigMaps, claims, ...) are all traced back correctly.
func resolveObjectServices(obj runtime.Object, ref manifest.ObjectRef, tracker *manifest.Tracker, ctx *buildManifestContext) []string {
	var names []string
	add := func(name string) {
		if name == "" {
			return
		}
		if _, ok := ctx.services[name]; !ok {
			return
		}
		names = appendUnique(names, name)
	}

	// 1. Events observed during the conversion itself.
	for _, ev := range tracker.EventsForObject(ref) {
		if ev.SourceType == manifest.SourceService {
			add(ev.Name)
		}
	}

	// 2. The standard io.kompose.service label / selector.
	if meta, ok := obj.(metav1.Object); ok {
		if name := meta.GetLabels()[transformer.Selector]; name != "" {
			add(name)
			for _, member := range ctx.groups[name] {
				add(member)
			}
		}
	}

	// 3. Containers embedded in workloads (grouped services and
	// network_mode: service:<x> merges).
	for _, container := range workloadContainers(obj) {
		add(ctx.containers[container])
	}

	// 4. Object name based matching for generated helpers.
	switch ref.Kind {
	case "ConfigMap":
		if svc, ok := ctx.envConfigMaps[ref.Name]; ok {
			add(svc)
		}
		for _, svc := range ctx.fileConfigMaps[ref.Name] {
			add(svc)
		}
		// ConfigMaps created from host paths for --volumes configMap are
		// named "<svc>-cm<N>"; fall back to the label resolution above.
	case "PersistentVolumeClaim":
		for _, svc := range ctx.claims[ref.Name] {
			add(svc)
		}
	case "Secret":
		for _, svc := range ctx.secretConsumers[ref.Name] {
			add(svc)
		}
	case "NetworkPolicy":
		for _, net := range networkPolicyNetworks(obj) {
			for _, svc := range ctx.networks[net] {
				add(svc)
			}
		}
	case "HorizontalPodAutoscaler":
		add(ref.Name)
	case "Service":
		// LoadBalancer splits into <name>-tcp / <name>-udp.
		for _, suffix := range []string{"-tcp", "-udp"} {
			if strings.HasSuffix(ref.Name, suffix) {
				add(strings.TrimSuffix(ref.Name, suffix))
			}
		}
		if isHeadlessService(obj) {
			add(ref.Name)
		}
	case "ImageStream", "BuildConfig", "Route", "Ingress":
		add(ref.Name)
	}

	sort.Strings(names)
	return names
}

// workloadContainers returns the container names embedded in a workload
// object, including init containers.
func workloadContainers(obj runtime.Object) []string {
	pod := podTemplateSpec(obj)
	if pod == nil {
		return nil
	}
	var names []string
	for _, c := range pod.Spec.Containers {
		names = append(names, c.Name)
	}
	for _, c := range pod.Spec.InitContainers {
		names = append(names, c.Name)
	}
	return names
}

func podTemplateSpec(obj runtime.Object) *corev1.PodTemplateSpec {
	switch t := obj.(type) {
	case *appsv1.Deployment:
		return &t.Spec.Template
	case *appsv1.DaemonSet:
		return &t.Spec.Template
	case *appsv1.StatefulSet:
		return &t.Spec.Template
	case *batchv1.CronJob:
		return &t.Spec.JobTemplate.Spec.Template
	case *corev1.Pod:
		return &corev1.PodTemplateSpec{ObjectMeta: t.ObjectMeta, Spec: t.Spec}
	case *deployapi.DeploymentConfig:
		return t.Spec.Template
	}
	return nil
}

func isHeadlessService(obj runtime.Object) bool {
	svc, ok := obj.(*corev1.Service)
	return ok && svc.Spec.ClusterIP == "None"
}

func networkPolicyNetworks(obj runtime.Object) []string {
	np, ok := obj.(*networkingv1.NetworkPolicy)
	if !ok {
		return nil
	}
	var networks []string
	for key := range np.Spec.PodSelector.MatchLabels {
		const prefix = "io.kompose.network/"
		if strings.HasPrefix(key, prefix) {
			networks = append(networks, strings.TrimPrefix(key, prefix))
		}
	}
	return networks
}

// buildServiceSource creates the service typed source for an object,
// including only the input field paths relevant to that object kind.
func buildServiceSource(svc kobject.ServiceConfig, kind string, obj runtime.Object, opt kobject.ConvertOptions, docs manifest.DocumentIndex) manifest.Source {
	paths, redacted := relevantFieldPaths(svc, kind, obj, opt)
	return manifest.Source{
		Type:           manifest.SourceService,
		Name:           svc.Name,
		Documents:      docs.ServiceDocuments(svc.Name),
		FieldPaths:     paths,
		ValuesRedacted: redacted,
	}
}

// relevantFieldPaths maps the populated fields of a ServiceConfig back to
// their Compose paths for the given object kind. Only structural paths are
// returned; values are never included.
func relevantFieldPaths(svc kobject.ServiceConfig, kind string, obj runtime.Object, opt kobject.ConvertOptions) ([]string, []string) {
	prefix := "services." + svc.Name + "."

	switch kind {
	case "Service":
		return serviceObjectFieldPaths(svc, prefix, obj), nil
	case "Ingress", "Route":
		return exposeFieldPaths(svc, prefix), nil
	case "PersistentVolumeClaim":
		return volumeFieldPaths(svc, prefix), nil
	case "ConfigMap":
		return configMapFieldPaths(svc, prefix)
	case "Secret":
		// Only the secret reference comes from the service definition; the
		// secret content itself lives under the top level "secrets:" key.
		var paths []string
		for _, secret := range svc.Secrets {
			paths = append(paths, prefix+"secrets."+secret.Source)
		}
		if len(svc.Secrets) > 0 {
			paths = append(paths, prefix+"secrets")
		}
		return manifest.UniqueStrings(paths), []string{redactedSecretData}
	case "NetworkPolicy":
		var paths []string
		if len(svc.Network) > 0 {
			paths = append(paths, prefix+"networks")
		}
		return paths, nil
	case "HorizontalPodAutoscaler":
		return hpaFieldPaths(svc, prefix), nil
	case "ImageStream":
		var paths []string
		if svc.Image != "" {
			paths = append(paths, prefix+"image")
		}
		if svc.Build != "" {
			paths = append(paths, prefix+"build")
		}
		return paths, nil
	case "BuildConfig":
		var paths []string
		if svc.Build != "" {
			paths = append(paths, prefix+"build")
		}
		if len(svc.BuildArgs) > 0 {
			paths = append(paths, prefix+"build.args")
		}
		if svc.Dockerfile != "" {
			paths = append(paths, prefix+"build.dockerfile")
		}
		if svc.BuildTarget != "" {
			paths = append(paths, prefix+"build.target")
		}
		return paths, []string{redactedEnvironment}
	}

	// Workload objects (Deployment, DaemonSet, StatefulSet, Pod, CronJob,
	// DeploymentConfig, ...) and any unknown kind carry the full set of
	// workload relevant input fields.
	return workloadFieldPaths(svc, prefix, opt)
}

func workloadFieldPaths(svc kobject.ServiceConfig, prefix string, opt kobject.ConvertOptions) ([]string, []string) {
	var paths []string
	add := func(condition bool, path string) {
		if condition {
			paths = append(paths, path)
		}
	}

	add(svc.Image != "", prefix+"image")
	add(len(svc.Command) > 0, prefix+"entrypoint")
	add(len(svc.Args) > 0, prefix+"command")
	add(svc.WorkingDir != "", prefix+"working_dir")
	add(len(svc.Port) > 0, prefix+"ports")
	add(len(svc.Expose) > 0, prefix+"expose")
	add(svc.ReadOnly, prefix+"read_only")
	add(len(svc.CapAdd) > 0, prefix+"cap_add")
	add(len(svc.CapDrop) > 0, prefix+"cap_drop")
	add(svc.Privileged, prefix+"privileged")
	add(svc.User != "", prefix+"user")
	add(svc.Restart != "", prefix+"restart")
	add(svc.Stdin, prefix+"stdin_open")
	add(svc.Tty, prefix+"tty")
	add(len(svc.TmpFs) > 0, prefix+"tmpfs")
	add(len(svc.VolList) > 0 || len(svc.Volumes) > 0, prefix+"volumes")
	add(svc.NetworkMode != "", prefix+"network_mode")
	add(len(svc.Network) > 0, prefix+"networks")
	add(svc.HostName != "", prefix+"hostname")
	add(svc.DomainName != "", prefix+"domainname")
	add(svc.Pid != "", prefix+"pid")
	add(svc.StopGracePeriod != "", prefix+"stop_grace_period")
	add(svc.Build != "", prefix+"build")
	add(svc.DeployMode != "", prefix+"deploy.mode")
	add(svc.Replicas != 0, prefix+"deploy.replicas")
	add(svc.Replicas == 0 && opt.Replicas > 0, "option.replicas")
	add(len(svc.Placement.PositiveConstraints)+len(svc.Placement.NegativeConstraints) > 0, prefix+"deploy.placement.constraints")
	add(len(svc.Placement.Preferences) > 0, prefix+"deploy.placement.preferences")
	add(svc.DeployUpdateConfig.Order != "", prefix+"deploy.update_config")
	add(len(svc.Secrets) > 0, prefix+"secrets")
	add(len(svc.Configs) > 0, prefix+"configs")
	add(len(svc.GroupAdd) > 0, prefix+"group_add")
	add(svc.FsGroup != 0, prefix+"labels.kompose.security-context.fsgroup")
	add(svc.CronJobSchedule != "", prefix+"labels.kompose.cronjob.schedule")
	add(svc.CPUShares != 0, prefix+"cpu_shares")
	add(svc.CPUQuota != 0, prefix+"cpu_quota")
	add(svc.CPULimit != 0 || svc.MemLimit != 0, prefix+"deploy.resources.limits")
	add(svc.MemLimit != 0, prefix+"mem_limit")
	add(svc.CPUReservation != 0 || svc.MemReservation != 0, prefix+"deploy.resources.reservations")
	add(svc.ImagePullSecret != "", prefix+"labels.kompose.image-pull-secret")
	add(svc.ImagePullPolicy != "", prefix+"labels.kompose.image-pull-policy")
	add(svc.VolumeMountSubPath != "", prefix+"labels.kompose.volume.subpath")
	add(svc.ExposeContainerToHost, prefix+"labels.kompose.controller.port.expose")
	add(svc.ServiceType != "", prefix+"labels.kompose.service.type")
	add(svc.ServiceExternalTrafficPolicy != "", prefix+"labels.kompose.service.external-traffic-policy")
	add(svc.NodePortPort != 0, prefix+"labels.kompose.service.nodeport.port")
	add(svc.ExposeService != "", prefix+"labels.kompose.service.expose")
	add(svc.ExposeServiceTLS != "", prefix+"labels.kompose.service.expose.tls-secret")
	add(svc.ExposeServiceIngressClassName != "", prefix+"labels.kompose.service.expose.ingress-class-name")
	add(len(svc.Labels) > 0, prefix+"labels")
	add(len(svc.Annotations) > 0, prefix+"labels")
	add(len(svc.BuildArgs) > 0, prefix+"build.args")
	add(len(svc.BuildLabels) > 0, prefix+"build.labels")
	add(svc.BuildTarget != "", prefix+"build.target")
	add(svc.Dockerfile != "", prefix+"build.dockerfile")
	add(svc.HealthChecks.Liveness.Test != nil || svc.HealthChecks.Liveness.HTTPPath != "" || svc.HealthChecks.Liveness.TCPPort != 0, prefix+"healthcheck")
	add(svc.HealthChecks.Readiness.HTTPPath != "" || svc.HealthChecks.Readiness.TCPPort != 0 || svc.HealthChecks.Readiness.Test != nil,
		prefix+"labels.kompose.service.healthcheck.readiness")

	// Environment variable NAMES are traceable; values are never recorded.
	for _, env := range svc.Environment {
		paths = append(paths, prefix+"environment."+env.Name)
	}
	add(len(svc.Environment) > 0, prefix+"environment")
	add(len(svc.EnvFile) > 0, prefix+"env_file")

	var redacted []string
	if len(svc.Environment) > 0 || len(svc.EnvFile) > 0 {
		redacted = append(redacted, redactedEnvironment)
	}
	if len(svc.Secrets) > 0 {
		redacted = append(redacted, redactedSecretData)
	}
	return manifest.UniqueStrings(paths), manifest.UniqueStrings(redacted)
}

func serviceObjectFieldPaths(svc kobject.ServiceConfig, prefix string, obj runtime.Object) []string {
	var paths []string
	if len(svc.Port) > 0 {
		paths = append(paths, prefix+"ports")
	}
	if len(svc.Expose) > 0 {
		paths = append(paths, prefix+"expose")
	}
	if svc.ServiceType != "" {
		paths = append(paths, prefix+"labels.kompose.service.type")
	}
	if svc.NodePortPort != 0 {
		paths = append(paths, prefix+"labels.kompose.service.nodeport.port")
	}
	if svc.ServiceExternalTrafficPolicy != "" {
		paths = append(paths, prefix+"labels.kompose.service.external-traffic-policy")
	}
	if isHeadlessService(obj) {
		// Headless Services are generated for StatefulSets even without ports.
		paths = append(paths, "option.controller=statefulset")
	}
	return manifest.UniqueStrings(paths)
}

func exposeFieldPaths(svc kobject.ServiceConfig, prefix string) []string {
	var paths []string
	if svc.ExposeService != "" {
		paths = append(paths, prefix+"labels.kompose.service.expose")
	}
	if svc.ExposeServicePath != "" {
		paths = append(paths, prefix+"labels.kompose.service.expose.path")
	}
	if svc.ExposeServiceTLS != "" {
		paths = append(paths, prefix+"labels.kompose.service.expose.tls-secret")
	}
	if svc.ExposeServiceIngressClassName != "" {
		paths = append(paths, prefix+"labels.kompose.service.expose.ingress-class-name")
	}
	if len(svc.Port) > 0 {
		paths = append(paths, prefix+"ports")
	}
	return paths
}

func volumeFieldPaths(svc kobject.ServiceConfig, prefix string) []string {
	var paths []string
	if len(svc.VolList) > 0 || len(svc.Volumes) > 0 {
		paths = append(paths, prefix+"volumes")
	}
	if _, ok := svc.Labels["kompose.volume.size"]; ok {
		paths = append(paths, prefix+"labels.kompose.volume.size")
	}
	if _, ok := svc.Labels["kompose.volume.storage-class-name"]; ok {
		paths = append(paths, prefix+"labels.kompose.volume.storage-class-name")
	}
	return paths
}

func configMapFieldPaths(svc kobject.ServiceConfig, prefix string) ([]string, []string) {
	var paths []string
	var redacted []string
	if len(svc.EnvFile) > 0 {
		paths = append(paths, prefix+"env_file")
		redacted = append(redacted, redactedEnvironment)
	}
	if len(svc.Environment) > 0 {
		paths = append(paths, prefix+"environment")
		redacted = append(redacted, redactedEnvironment)
	}
	if len(svc.Configs) > 0 {
		paths = append(paths, prefix+"configs")
	}
	if len(svc.Volumes) > 0 || len(svc.VolList) > 0 {
		paths = append(paths, prefix+"volumes")
	}
	return manifest.UniqueStrings(paths), manifest.UniqueStrings(redacted)
}

func hpaFieldPaths(svc kobject.ServiceConfig, prefix string) []string {
	var paths []string
	hpaLabels := []string{
		compose.LabelHpaCPU, compose.LabelHpaMemory,
		compose.LabelHpaMinReplicas, compose.LabelHpaMaxReplicas,
	}
	for _, label := range hpaLabels {
		if _, ok := svc.Labels[label]; ok {
			paths = append(paths, prefix+"labels."+label)
		}
	}
	return paths
}

// extraSources resolves sources that are not tied to a service (top level
// keys or command line options).
func extraSources(obj runtime.Object, ref manifest.ObjectRef, ko kobject.KomposeObject) []manifest.Source {
	switch ref.Kind {
	case "Namespace":
		return []manifest.Source{{
			Type:       manifest.SourceOption,
			Name:       "--namespace",
			FieldPaths: []string{"option.namespace"},
		}}
	case "Secret":
		return secretSources(ref, ko)
	}
	return nil
}

func secretSources(ref manifest.ObjectRef, ko kobject.KomposeObject) []manifest.Source {
	var sources []manifest.Source
	for name, cfg := range ko.Secrets {
		if FormatResourceName(name) != ref.Name {
			continue
		}
		path := "secrets." + name
		fieldPath := path + ".file"
		if cfg.File == "" {
			fieldPath = path + ".external"
		}
		sources = append(sources, manifest.Source{
			Type:           manifest.SourceRoot,
			Name:           "secrets",
			Item:           name,
			FieldPaths:     []string{fieldPath},
			ValuesRedacted: []string{redactedSecretData},
		})
		break
	}
	if len(sources) == 0 {
		sources = append(sources, manifest.Source{
			Type:           manifest.SourceRoot,
			Name:           "secrets",
			Item:           ref.Name,
			FieldPaths:     []string{"secrets." + ref.Name},
			ValuesRedacted: []string{redactedSecretData},
		})
	}
	return sources
}

// missingSecretWarning returns a warning when a pod template references a
// Secret volume that has no file backed declaration in the Compose project.
func missingSecretWarning(pod *corev1.PodTemplateSpec, ko kobject.KomposeObject) string {
	declared := map[string]bool{}
	for name := range ko.Secrets {
		declared[FormatResourceName(name)] = true
	}
	for _, vol := range pod.Spec.Volumes {
		if vol.Secret == nil {
			continue
		}
		if declared[vol.Secret.SecretName] {
			continue
		}
		return fmt.Sprintf("pod references Secret %q which is not declared as a file backed entry under the Compose top level \"secrets:\" key", vol.Secret.SecretName)
	}
	return ""
}

// resolveStages returns the ordered, deduplicated list of conversion stages
// for an object, combining the events observed on the tracker with the
// stages every object passes through.
func resolveStages(tracker *manifest.Tracker, ref manifest.ObjectRef, sources []manifest.Source) []string {
	seen := map[string]bool{}
	var stages []string
	add := func(stage string) {
		if stage == "" || seen[stage] {
			return
		}
		seen[stage] = true
		stages = append(stages, stage)
	}

	hasLoadableSource := false
	for _, s := range sources {
		if s.Type == manifest.SourceService || s.Type == manifest.SourceRoot {
			hasLoadableSource = true
		}
	}
	if hasLoadableSource {
		add(manifest.StageLoad)
	}

	var observed []string
	for _, ev := range tracker.EventsForObject(ref) {
		if !seen[ev.Stage] {
			observed = appendUnique(observed, ev.Stage)
		}
	}
	sort.SliceStable(observed, func(i, j int) bool {
		return stageOrder(observed[i]) < stageOrder(observed[j])
	})
	for _, stage := range observed {
		add(stage)
	}

	add(manifest.StageAggregate)
	add(manifest.StageOutput)
	return stages
}

func stageOrder(stage string) int {
	switch stage {
	case manifest.StageLoad:
		return 0
	case manifest.StageTransformInitialize:
		return 1
	case manifest.StageTransformPopulate:
		return 2
	case manifest.StageAggregate:
		return 3
	case manifest.StageOutput:
		return 4
	default:
		return 2
	}
}
