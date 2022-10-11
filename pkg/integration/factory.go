/*
Copyright 2022 The KubeVela Authors.

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

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	pkgtypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/klog"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	"github.com/oam-dev/kubevela/apis/core.oam.dev/common"
	"github.com/oam-dev/kubevela/apis/core.oam.dev/v1alpha1"
	"github.com/oam-dev/kubevela/apis/core.oam.dev/v1beta1"
	"github.com/oam-dev/kubevela/apis/types"
	"github.com/oam-dev/kubevela/pkg/apiserver/domain/model"
	"github.com/oam-dev/kubevela/pkg/cue"
	"github.com/oam-dev/kubevela/pkg/cue/script"
	icontext "github.com/oam-dev/kubevela/pkg/integration/context"
	"github.com/oam-dev/kubevela/pkg/integration/writer"
	"github.com/oam-dev/kubevela/pkg/utils/apply"
)

// SaveInputPropertiesKey define the key name for saving the input properties in the secret
const SaveInputPropertiesKey = "input-properties"

// SaveObjectReference define the key name for saving the outputs objects reference metadata in the secret
const SaveObjectReference = "objects-reference"

// TemplateConfigMapNamePrefix the prefix of the configmap name
const TemplateConfigMapNamePrefix = "integration-template-"

// ErrSensitiveIntegration means this integration can not be read directly.
var ErrSensitiveIntegration = fmt.Errorf("the integration is sensitive")

// ErrNoIntegrationOrTarget means the integration or the target is empty
var ErrNoIntegrationOrTarget = fmt.Errorf("the integration or the target is empty")

// NamespacedName the namespace and name model
type NamespacedName struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
}

// Template This is the spec of the integration template, parse from the cue script.
type Template struct {
	NamespacedName
	Alias       string `json:"alias"`
	Description string `json:"description"`
	// Scope defines the usage scope of the configuration template. Provides two options: System or Namespace
	// System: The system users could use this template, and the integration secret will save in the vela-system namespace.
	// Namespace: The integration secret will save in the target namespace, such as this namespace belonging to one project.
	Scope string `json:"scope"`
	// Sensitive means this integration config can not be read from the API or the workflow step, only support the safe way, such as Secret.
	Sensitive bool `json:"sensitive"`

	CreateTime time.Time `json:"createTime"`

	Template script.CUE `json:"template"`

	ExpandedWriter writer.ExpandedWriterConfig `json:"expandedWriter"`

	Schema *openapi3.Schema `json:"schema"`

	ConfigMap *v1.ConfigMap `json:"-"`
}

// Metadata users should provide this model.
type Metadata struct {
	NamespacedName
	Alias       string                 `json:"alias"`
	Description string                 `json:"description"`
	Properties  map[string]interface{} `json:"properties"`
}

// Integration this is the integration model, generated from the template and properties.
type Integration struct {
	Metadata
	CreateTime time.Time
	Template   Template `json:"template"`
	// Secret this is default output way.
	Secret *v1.Secret `json:"secret"`

	// ExpandedWriterData
	ExpandedWriterData *writer.ExpandedWriterData `json:"expandedWriterData"`

	// OutputObjects this means users could define other objects.
	OutputObjects map[string]*unstructured.Unstructured
}

// ClusterTarget kubernetes delivery target
type ClusterTarget struct {
	ClusterName string `json:"clusterName"`
	Namespace   string `json:"namespace"`
}

// Distribution the integration distribution model
type Distribution struct {
	Name         string                  `json:"name"`
	Namespace    string                  `json:"namespace"`
	CreatedTime  time.Time               `json:"createdTime"`
	Integrations []*NamespacedName       `json:"integrations"`
	Targets      []*ClusterTarget        `json:"targets"`
	Application  pkgtypes.NamespacedName `json:"application"`
	Status       common.AppStatus        `json:"status"`
}

// ApplyDistributionSpec the spec of the distribution
type ApplyDistributionSpec struct {
	Integrations []*NamespacedName
	Targets      []*ClusterTarget
}

// Factory handle the integration
type Factory interface {
	ParseTemplate(defaultName string, content []byte) (*Template, error)
	ParseIntegration(ctx context.Context, template NamespacedName, meta Metadata) (*Integration, error)

	LoadTemplate(ctx context.Context, name, ns string) (*Template, error)
	ApplyTemplate(ctx context.Context, ns string, it *Template) error
	DeleteTemplate(ctx context.Context, ns, name string) error
	ListTemplates(ctx context.Context, ns, scope string) ([]*Template, error)

	ReadIntegration(ctx context.Context, namespace, name string) (map[string]interface{}, error)
	GetIntegration(ctx context.Context, namespace, name string) (*Integration, error)
	ListIntegrations(ctx context.Context, namespace, template, scope string) ([]*Integration, error)
	DeleteIntegration(ctx context.Context, namespace, name string) error
	ApplyIntegration(ctx context.Context, i *Integration, ns string) error

	ApplyDistribution(ctx context.Context, ns, name string, ads *ApplyDistributionSpec) error
	ListDistributions(ctx context.Context, ns string) ([]*Distribution, error)
	DeleteDistribution(ctx context.Context, ns, name string) error
}

// NewIntegrationFactory create a integration factory instance
func NewIntegrationFactory(cli client.Client) Factory {
	return &kubeIntegrationFactory{cli: cli, apiApply: apply.NewAPIApplicator(cli)}
}

type kubeIntegrationFactory struct {
	cli      client.Client
	apiApply *apply.APIApplicator
}

// ParseTemplate parse a integration template instance form the cue script
func (k *kubeIntegrationFactory) ParseTemplate(defaultName string, content []byte) (*Template, error) {
	cueScript := script.BuildCUEScriptWithDefaultContext(icontext.DefaultContext, content)

	value, err := cueScript.ParseToValue(false)
	if err != nil {
		return nil, fmt.Errorf("the cue script is invalid:%w", err)
	}
	name, err := value.GetString("metadata", "name")
	if err != nil {
		if defaultName == "" {
			return nil, fmt.Errorf("fail to get the name from the template metadata: %w", err)
		}
	}
	if defaultName != "" {
		name = defaultName
	}
	schema, err := cueScript.ParsePropertiesToSchema()
	if err != nil {
		return nil, fmt.Errorf("the properties of the cue script is invalid:%w", err)
	}
	alias, err := value.GetString("metadata", "alias")
	if err != nil && !IsFieldNotExist(err) {
		klog.Warningf("fail to get the alias from the template metadata: %s", err.Error())
	}
	scope, err := value.GetString("metadata", "scope")
	if err != nil && !IsFieldNotExist(err) {
		klog.Warningf("fail to get the scope from the template metadata: %s", err.Error())
	}
	sensitive, err := value.GetBool("metadata", "sensitive")
	if err != nil && !IsFieldNotExist(err) {
		klog.Warningf("fail to get the scope from the template metadata: %s", err.Error())
	}
	templateValue, err := value.LookupValue("template")
	if err != nil && !IsFieldNotExist(err) {
		klog.Warningf("fail to get the scope from the template metadata: %s", err.Error())
	}
	template := &Template{
		NamespacedName: NamespacedName{
			Name: name,
		},
		Alias:          alias,
		Scope:          scope,
		Sensitive:      sensitive,
		Template:       cueScript,
		Schema:         schema,
		ExpandedWriter: writer.ParseExpandedWriterConfig(templateValue),
	}

	var configmap v1.ConfigMap
	configmap.Name = TemplateConfigMapNamePrefix + template.Name

	configmap.Data = map[string]string{
		"template": string(template.Template),
	}
	if template.Schema != nil {
		data, err := yaml.Marshal(template.Schema)
		if err != nil {
			return nil, err
		}
		configmap.Data["schema"] = string(data)
	}
	data, err := yaml.Marshal(template.ExpandedWriter)
	if err != nil {
		return nil, err
	}
	configmap.Data["expanded-writer"] = string(data)
	configmap.Labels = map[string]string{
		types.LabelConfigCatalog: "integration",
		types.LabelConfigScope:   template.Scope,
	}
	configmap.Annotations = map[string]string{
		types.AnnotationConfigDescription:    template.Description,
		types.AnnotationConfigAlias:          template.Alias,
		types.AnnotationIntegrationSensitive: fmt.Sprintf("%t", template.Sensitive),
	}
	template.ConfigMap = &configmap

	return template, nil
}

// IsFieldNotExist check whether the error type is the field not found
func IsFieldNotExist(err error) bool {
	return strings.Contains(err.Error(), "not exist")
}

// ApplyTemplate parse and update the integration template
func (k *kubeIntegrationFactory) ApplyTemplate(ctx context.Context, ns string, it *Template) error {
	it.ConfigMap.Namespace = ns
	c, cancel := context.WithTimeout(ctx, time.Second*10)
	defer cancel()
	return k.apiApply.Apply(c, it.ConfigMap, apply.DisableUpdateAnnotation())
}

func convertConfigMap2Template(cm v1.ConfigMap) (*Template, error) {
	if cm.Labels == nil || cm.Annotations == nil {
		return nil, fmt.Errorf("this configmap is not a valid template")
	}
	it := &Template{
		NamespacedName: NamespacedName{
			Name:      strings.Replace(cm.Name, TemplateConfigMapNamePrefix, "", 1),
			Namespace: cm.Namespace,
		},
		Alias:       cm.Annotations[types.AnnotationConfigAlias],
		Description: cm.Annotations[types.AnnotationConfigDescription],
		Sensitive:   cm.Annotations[types.AnnotationIntegrationSensitive] == "true",
		Scope:       cm.Labels[types.LabelConfigScope],
		CreateTime:  cm.CreationTimestamp.Time,
		Template:    script.CUE(cm.Data["template"]),
	}
	if cm.Data["schema"] != "" {
		var schema openapi3.Schema
		err := yaml.Unmarshal([]byte(cm.Data["schema"]), &schema)
		if err != nil {
			return nil, fmt.Errorf("fail to parse the schema: %w", err)
		}
		it.Schema = &schema
	}
	if cm.Data["expanded-writer"] != "" {
		var config writer.ExpandedWriterConfig
		err := yaml.Unmarshal([]byte(cm.Data["expanded-writer"]), &config)
		if err != nil {
			return nil, fmt.Errorf("fail to parse the schema: %w", err)
		}
		it.ExpandedWriter = config
	}
	return it, nil
}

// DeleteTemplate delete the integration template
func (k *kubeIntegrationFactory) DeleteTemplate(ctx context.Context, ns, name string) error {
	var configmap v1.ConfigMap
	c, cancel := context.WithTimeout(ctx, time.Second*10)
	defer cancel()
	if err := k.cli.Get(c, pkgtypes.NamespacedName{Namespace: ns, Name: TemplateConfigMapNamePrefix + name}, &configmap); err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("the integration template %s not found", name)
		}
		return fmt.Errorf("fail to delete the integration template %s:%w", name, err)
	}
	return k.cli.Delete(c, &configmap)
}

// ListTemplates list the integration templates
func (k *kubeIntegrationFactory) ListTemplates(ctx context.Context, ns, scope string) ([]*Template, error) {
	c, cancel := context.WithTimeout(ctx, time.Minute*1)
	defer cancel()
	var list = &v1.ConfigMapList{}
	selector, err := labels.Parse(fmt.Sprintf("%s=%s", types.LabelConfigCatalog, "integration"))
	if err != nil {
		return nil, err
	}
	if err := k.cli.List(c, list,
		client.MatchingLabelsSelector{Selector: selector},
		client.InNamespace(ns)); err != nil {
		return nil, err
	}
	var templates []*Template
	for _, item := range list.Items {
		it, err := convertConfigMap2Template(item)
		if err != nil {
			klog.Warningf("fail to parse the configmap %s:%s", item.Name, err.Error())
		}
		if it != nil {
			if scope == "" || it.Scope == scope {
				templates = append(templates, it)
			}
		}
	}
	return templates, nil
}

// LoadTemplate load the template
func (k *kubeIntegrationFactory) LoadTemplate(ctx context.Context, name, ns string) (*Template, error) {
	var cm v1.ConfigMap
	if err := k.cli.Get(ctx, pkgtypes.NamespacedName{Namespace: ns, Name: TemplateConfigMapNamePrefix + name}, &cm); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("the integration template %s/%s not found", ns, name)
		}
		return nil, err
	}
	return convertConfigMap2Template(cm)
}

// ParseIntegration merge the properties to template and build a integration instance
// If the templateName is empty, means creating a secret without the template.
func (k *kubeIntegrationFactory) ParseIntegration(ctx context.Context,
	template NamespacedName, meta Metadata,
) (*Integration, error) {
	var secret v1.Secret

	integration := &Integration{
		Metadata: meta,
		Secret:   &secret,
	}

	if template.Name != "" {
		template, err := k.LoadTemplate(ctx, template.Name, template.Namespace)
		if err != nil {
			return nil, err
		}
		contextValue := icontext.IntegrationRenderContext{
			Name:      meta.Name,
			Namespace: meta.Namespace,
		}
		// Render the output secret
		output, err := template.Template.RunAndOutput(contextValue, meta.Properties)
		if err != nil && !cue.IsFieldNotExist(err) {
			return nil, err
		}
		if output != nil {
			if err := output.UnmarshalTo(&secret); err != nil {
				return nil, fmt.Errorf("the output format must be secret")
			}
		}
		if secret.Type == "" {
			secret.Type = v1.SecretType(fmt.Sprintf("%s/%s", "", template.Name))
		}
		if secret.Labels == nil {
			secret.Labels = map[string]string{}
		}
		secret.Labels[types.LabelConfigCatalog] = types.CatalogIntegration
		secret.Labels[types.LabelConfigType] = template.Name
		secret.Labels[types.LabelConfigType] = template.Name
		secret.Labels[types.LabelConfigScope] = template.Scope

		if secret.Annotations == nil {
			secret.Annotations = map[string]string{}
		}
		secret.Annotations[types.AnnotationIntegrationSensitive] = fmt.Sprintf("%t", template.Sensitive)
		secret.Annotations[types.AnnotationIntegrationTemplateNamespace] = template.Namespace
		integration.Template = *template

		// Render the expanded writer configuration
		data, err := writer.RenderForExpandedWriter(template.ExpandedWriter, integration.Template.Template, contextValue, meta.Properties)
		if err != nil {
			return nil, fmt.Errorf("fail to render the content for the expanded writer:%w ", err)
		}
		integration.ExpandedWriterData = data

		// Render the outputs objects
		outputs, err := template.Template.RunAndOutput(contextValue, meta.Properties, "template", "outputs")
		if err != nil && !cue.IsFieldNotExist(err) {
			return nil, err
		}
		if outputs != nil {
			var objects = map[string]interface{}{}
			if err := outputs.UnmarshalTo(&objects); err != nil {
				return nil, fmt.Errorf("the outputs is invalid %w", err)
			}
			var objectReferences []v1.ObjectReference
			integration.OutputObjects = make(map[string]*unstructured.Unstructured)
			for k := range objects {
				if ob, ok := objects[k].(map[string]interface{}); ok {
					obj := &unstructured.Unstructured{Object: ob}
					integration.OutputObjects[k] = obj
					objectReferences = append(objectReferences, v1.ObjectReference{
						Kind:       obj.GetKind(),
						Namespace:  obj.GetNamespace(),
						Name:       obj.GetName(),
						APIVersion: obj.GetAPIVersion(),
					})
				}
			}
			objectReferenceJSON, err := json.Marshal(objectReferences)
			if err != nil {
				return nil, err
			}
			if secret.Data == nil {
				secret.Data = map[string][]byte{}
			}
			secret.Data[SaveObjectReference] = objectReferenceJSON
		}
	} else {
		secret.Labels = map[string]string{
			types.LabelConfigCatalog: "integration",
			types.LabelConfigType:    "",
		}
		secret.Annotations = map[string]string{}
	}
	secret.Namespace = meta.Namespace
	if secret.Name == "" {
		secret.Name = meta.Name
	}
	secret.Annotations[types.AnnotationConfigAlias] = meta.Alias
	secret.Annotations[types.AnnotationConfigDescription] = meta.Description
	pp, err := json.Marshal(meta.Properties)
	if err != nil {
		return nil, err
	}
	if secret.Data == nil {
		secret.Data = map[string][]byte{}
	}
	secret.Data[SaveInputPropertiesKey] = pp

	return integration, nil
}

// ReadIntegration read the integration secret
func (k *kubeIntegrationFactory) ReadIntegration(ctx context.Context, namespace, name string) (map[string]interface{}, error) {
	var secret v1.Secret
	c, cancel := context.WithTimeout(ctx, time.Second*10)
	defer cancel()
	if err := k.cli.Get(c, pkgtypes.NamespacedName{Namespace: namespace, Name: name}, &secret); err != nil {
		return nil, err
	}
	if secret.Annotations[types.AnnotationIntegrationSensitive] == "true" {
		return nil, ErrSensitiveIntegration
	}
	properties := secret.Data[SaveInputPropertiesKey]
	var input = map[string]interface{}{}
	if err := json.Unmarshal(properties, &input); err != nil {
		return nil, err
	}
	return input, nil
}

func (k *kubeIntegrationFactory) GetIntegration(ctx context.Context, namespace, name string) (*Integration, error) {
	var secret v1.Secret
	c, cancel := context.WithTimeout(ctx, time.Second*10)
	defer cancel()
	if err := k.cli.Get(c, pkgtypes.NamespacedName{Namespace: namespace, Name: name}, &secret); err != nil {
		return nil, err
	}
	if secret.Annotations[types.AnnotationIntegrationSensitive] == "true" {
		return nil, ErrSensitiveIntegration
	}
	return convertSecret2Integration(&secret)
}

// Apply the integration secret to the Kube API server.
// Apply the expand output to the target server.
func (k *kubeIntegrationFactory) ApplyIntegration(ctx context.Context, i *Integration, ns string) error {
	c, cancel := context.WithTimeout(ctx, time.Second*10)
	defer cancel()
	if err := k.apiApply.Apply(c, i.Secret); err != nil {
		return fmt.Errorf("fail to apply the secret: %w", err)
	}
	for key, obj := range i.OutputObjects {
		obj.SetOwnerReferences([]metav1.OwnerReference{{
			APIVersion: "v1",
			Kind:       "Secret",
			Name:       i.Secret.Name,
			UID:        i.Secret.UID,
		}})
		if err := k.apiApply.Apply(c, obj); err != nil {
			return fmt.Errorf("fail to apply the object %s: %w", key, err)
		}
	}
	readIntegration := func(ctx context.Context, namespace, name string) (map[string]interface{}, error) {
		return k.ReadIntegration(ctx, namespace, name)
	}
	if i.ExpandedWriterData != nil {
		if errs := writer.Write(ctx, i.ExpandedWriterData, readIntegration); len(errs) > 0 {
			return errs[0]
		}
	}
	return nil
}

func (k *kubeIntegrationFactory) ListIntegrations(ctx context.Context, namespace, template, scope string) ([]*Integration, error) {
	c, cancel := context.WithTimeout(ctx, time.Minute*3)
	defer cancel()
	var list = &v1.SecretList{}
	requirement := fmt.Sprintf("%s=%s", types.LabelConfigCatalog, "integration")
	if template != "" {
		requirement = fmt.Sprintf("%s,%s=%s", requirement, types.LabelConfigType, template)
	}
	if scope != "" {
		requirement = fmt.Sprintf("%s,%s=%s", requirement, types.LabelConfigScope, scope)
	}
	selector, err := labels.Parse(requirement)
	if err != nil {
		return nil, err
	}
	if err := k.cli.List(c, list,
		client.MatchingLabelsSelector{Selector: selector},
		client.InNamespace(namespace)); err != nil {
		return nil, err
	}
	var integrations []*Integration
	for i := range list.Items {
		item := list.Items[i]
		it, err := convertSecret2Integration(&item)
		if err != nil {
			klog.Warningf("fail to parse the secret %s:%s", item.Name, err.Error())
		}
		if it != nil {
			integrations = append(integrations, it)
		}
	}
	return integrations, nil
}

func (k *kubeIntegrationFactory) DeleteIntegration(ctx context.Context, namespace, name string) error {
	var secret v1.Secret
	c, cancel := context.WithTimeout(ctx, time.Second*10)
	defer cancel()
	if err := k.cli.Get(c, pkgtypes.NamespacedName{Namespace: namespace, Name: name}, &secret); err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("the integration %s not found", name)
		}
		return fmt.Errorf("fail to delete the integration %s:%w", name, err)
	}
	if secret.Labels[types.LabelConfigCatalog] != "integration" {
		return fmt.Errorf("found a secret but is not a integration")
	}

	if objects, exist := secret.Data[SaveObjectReference]; exist {
		var objectReferences []v1.ObjectReference
		if err := json.Unmarshal(objects, &objectReferences); err != nil {
			return err
		}
		for _, obj := range objectReferences {
			if err := k.cli.Delete(ctx, convertObjectReference2Unstructured(obj)); err != nil && !apierrors.IsNotFound(err) {
				return fmt.Errorf("fail to clear the object %s:%w", obj.Name, err)
			}
		}
	}

	return k.cli.Delete(c, &secret)
}

func (k *kubeIntegrationFactory) ApplyDistribution(ctx context.Context, ns, name string, ads *ApplyDistributionSpec) error {
	policies := convertTarget2TopologyPolicy(ads.Targets)
	if len(policies) == 0 {
		return ErrNoIntegrationOrTarget
	}
	// create the share policy
	shareSpec := v1alpha1.SharedResourcePolicySpec{
		Rules: []v1alpha1.SharedResourcePolicyRule{{
			Selector: v1alpha1.ResourcePolicyRuleSelector{
				CompNames: []string{name},
			},
		}},
	}
	properties, err := json.Marshal(shareSpec)
	if err == nil {
		policies = append(policies, v1beta1.AppPolicy{
			Type: v1alpha1.SharedResourcePolicyType,
			Name: "share-integration",
			Properties: &runtime.RawExtension{
				Raw: properties,
			},
		})
	}

	var objects []map[string]string
	for _, s := range ads.Integrations {
		objects = append(objects, map[string]string{
			"name":      s.Name,
			"namespace": s.Namespace,
			"resource":  "secret",
		})
	}
	if len(objects) == 0 {
		return ErrNoIntegrationOrTarget
	}

	objectsBytes, err := json.Marshal(map[string][]map[string]string{"objects": objects})
	if err != nil {
		return err
	}

	reqByte, err := json.Marshal(ads)
	if err != nil {
		return err
	}

	distribution := &v1beta1.Application{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			Labels: map[string]string{
				model.LabelSourceOfTruth: model.FromInner,
				types.LabelConfigCatalog: types.CatalogIntegrationDistribution,
			},
			Annotations: map[string]string{
				types.AnnotationIntegrationDistributionSpec: string(reqByte),
			},
		},
		Spec: v1beta1.ApplicationSpec{
			Components: []common.ApplicationComponent{
				{
					Name:       name,
					Type:       v1alpha1.RefObjectsComponentType,
					Properties: &runtime.RawExtension{Raw: objectsBytes},
				},
			},
			Policies: policies,
		},
	}
	return k.apiApply.Apply(ctx, distribution)
}

func (k *kubeIntegrationFactory) ListDistributions(ctx context.Context, ns string) ([]*Distribution, error) {
	var apps v1beta1.ApplicationList
	if err := k.cli.List(ctx, &apps, client.MatchingLabels{
		model.LabelSourceOfTruth: model.FromInner,
		types.LabelConfigCatalog: types.CatalogIntegrationDistribution,
	}, client.InNamespace(ns)); err != nil {
		return nil, err
	}
	var list []*Distribution
	for _, app := range apps.Items {
		dis := &Distribution{
			Name:        app.Name,
			Namespace:   app.Namespace,
			CreatedTime: app.CreationTimestamp.Time,
			Application: pkgtypes.NamespacedName{
				Namespace: app.Namespace,
				Name:      app.Name,
			},
			Status: app.Status,
		}
		if spec, ok := app.Annotations[types.AnnotationIntegrationDistributionSpec]; ok {
			var req ApplyDistributionSpec
			if err := json.Unmarshal([]byte(spec), &req); err == nil {
				dis.Targets = req.Targets
				dis.Integrations = req.Integrations
			}
		}
		list = append(list, dis)
	}
	return list, nil
}
func (k *kubeIntegrationFactory) DeleteDistribution(ctx context.Context, ns, name string) error {
	app := &v1beta1.Application{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns,
			Name:      name,
		},
	}
	return k.cli.Delete(ctx, app)
}

func convertTarget2TopologyPolicy(targets []*ClusterTarget) (policies []v1beta1.AppPolicy) {
	for _, target := range targets {
		policySpec := v1alpha1.TopologyPolicySpec{
			Placement: v1alpha1.Placement{
				Clusters: []string{target.ClusterName},
			},
			Namespace: target.Namespace,
		}
		properties, err := json.Marshal(policySpec)
		if err == nil {
			policies = append(policies, v1beta1.AppPolicy{
				Type: v1alpha1.TopologyPolicyType,
				Name: fmt.Sprintf("%s-%s", target.ClusterName, target.Namespace),
				Properties: &runtime.RawExtension{
					Raw: properties,
				},
			})
		}
	}
	return
}

func convertSecret2Integration(se *v1.Secret) (*Integration, error) {
	if se == nil || se.Labels == nil {
		return nil, fmt.Errorf("this secret is not a valid integration secret")
	}
	integration := &Integration{
		Metadata: Metadata{
			NamespacedName: NamespacedName{
				Name:      se.Name,
				Namespace: se.Namespace,
			},
		},
		CreateTime: se.CreationTimestamp.Time,
		Secret:     se,
		Template: Template{
			NamespacedName: NamespacedName{
				Name: se.Labels[types.LabelConfigType],
			},
		},
	}
	if se.Annotations != nil {
		integration.Alias = se.Annotations[types.AnnotationConfigAlias]
		integration.Description = se.Annotations[types.AnnotationConfigDescription]
		integration.Template.Namespace = se.Annotations[types.AnnotationIntegrationTemplateNamespace]
		integration.Template.Sensitive = se.Annotations[types.AnnotationIntegrationSensitive] == "true"
	}
	if !integration.Template.Sensitive && len(se.Data[SaveInputPropertiesKey]) > 0 {
		var properties = map[string]interface{}{}
		if err := yaml.Unmarshal(se.Data[SaveInputPropertiesKey], &properties); err != nil {
			return nil, err
		}
		integration.Properties = properties
	}
	if !integration.Template.Sensitive {
		integration.Secret = se
	} else {
		seCope := se.DeepCopy()
		seCope.Data = nil
		seCope.StringData = nil
		integration.Secret = seCope
	}
	return integration, nil
}

func convertObjectReference2Unstructured(ref v1.ObjectReference) *unstructured.Unstructured {
	var obj unstructured.Unstructured
	obj.SetAPIVersion(ref.APIVersion)
	obj.SetNamespace(ref.Namespace)
	obj.SetKind(ref.Kind)
	obj.SetName(ref.Name)
	return &obj
}
