/*
Copyright 2018 The Kubernetes Authors.

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

package generators

import (
	"io"
	"text/template"
	"sort"

	clientgentypes "k8s.io/code-generator/cmd/client-gen/types"
	"k8s.io/gengo/generator"
	"k8s.io/gengo/types"
)

type registerExternalGenerator struct {
	generator.DefaultGen
	gv              clientgentypes.GroupVersion
	typesToGenerate []*types.Type
}

var _ generator.Generator = &registerExternalGenerator{}

func (g *registerExternalGenerator) Filter(_ *generator.Context, _ *types.Type) bool {
	return false
}

func (g *registerExternalGenerator) Finalize(context *generator.Context, w io.Writer) error {
	typesToGenerateOnlyNames := make([]string, len(g.typesToGenerate))
	for index, typeToGenerate := range g.typesToGenerate {
		typesToGenerateOnlyNames[index] = typeToGenerate.Name.Name
	}

	registerTemplateParam := struct {
		clientgentypes.GroupVersion
		TypesToGenerate []string
	}{
		g.gv,
		typesToGenerateOnlyNames,
	}

	// sort the list of types to register, so that the generator produces stable output
	sort.Strings(registerTemplateParam.TypesToGenerate)

	temp := template.Must(template.New("register-template").Parse(registerExternalTypesTemplate))
	return temp.Execute(w, registerTemplateParam)
}

var registerExternalTypesTemplate = `
// GroupName specifies the group name used to register the objects.
const GroupName = "{{.Group}}"

// GroupVersion specifies the group and the version used to register the objects.
var GroupVersion = v1.GroupVersion{Group: GroupName, Version: "{{.Version}}"}

// SchemeGroupVersion is group version used to register these objects
// Deprecated: use GroupName instead.
var SchemeGroupVersion = schema.GroupVersion{Group: GroupName, Version: "{{.Version}}"}

// Resource takes an unqualified resource and returns a Group qualified GroupResource
func Resource(resource string) schema.GroupResource {
	return SchemeGroupVersion.WithResource(resource).GroupResource()
}

var (
	// localSchemeBuilder and AddToScheme will stay in k8s.io/kubernetes.
	SchemeBuilder      runtime.SchemeBuilder
	localSchemeBuilder = &SchemeBuilder
    // Depreciated: use Install instead
	AddToScheme        = localSchemeBuilder.AddToScheme
	Install            = localSchemeBuilder.AddToScheme
)

func init() {
	// We only register manually written functions here. The registration of the
	// generated functions takes place in the generated files. The separation
	// makes the code compile even when the generated files are missing.
	localSchemeBuilder.Register(addKnownTypes)
}

// Adds the list of known types to Scheme.
func addKnownTypes(scheme *runtime.Scheme) error {
	scheme.AddKnownTypes(SchemeGroupVersion,
    {{ range .TypesToGenerate -}}
		&{{.}}{},
    {{ end -}}
	)
    // AddToGroupVersion allows the serialization of client types like ListOptions.
	v1.AddToGroupVersion(scheme, SchemeGroupVersion)
	return nil
}
`
