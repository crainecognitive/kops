/*
Copyright 2019 The Kubernetes Authors.

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

package utils

import (
	"sigs.k8s.io/yaml"
)

// See http://ghodss.com/2014/the-right-way-to-handle-yaml-in-golang/

// YAMLToJSON converts yaml to json
func YAMLToJSON(yamlBytes []byte) ([]byte, error) {
	return yaml.YAMLToJSON(yamlBytes)
}

// YamlUnmarshal unmarshals the yaml content to an interface
// Note: if you are loading a kops.k8s.io API object,
// normally you want something like kopscodecs.Decode,
// so that we can convert between apiVersions.
func YamlUnmarshal(yamlBytes []byte, dest interface{}) error {
	return yaml.Unmarshal(yamlBytes, dest)
}

// YamlMarshal tries to marshal the input struct into yaml content
func YamlMarshal(o interface{}) ([]byte, error) {
	return yaml.Marshal(o)
}
