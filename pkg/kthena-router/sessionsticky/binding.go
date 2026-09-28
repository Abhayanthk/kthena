/*
Copyright The Volcano Authors.

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

package sessionsticky

import "fmt"

// Redis hash field names for a session sticky binding.
const (
	redisFieldModelServer = "modelServer"
	redisFieldPod         = "pod"
)

// Binding is the stored affinity target: ModelServer short name (same
// namespace as the ModelServer that owns the sticky spec) and the selected Pod name.
type Binding struct {
	ModelServer string
	Pod         string
}

// Valid reports whether both fields are set.
func (b Binding) Valid() bool {
	return b.ModelServer != "" && b.Pod != ""
}

// Equal reports whether two bindings refer to the same server and pod.
func (b Binding) Equal(other Binding) bool {
	return b.ModelServer == other.ModelServer && b.Pod == other.Pod
}

// String returns a debug representation.
func (b Binding) String() string {
	if !b.Valid() {
		return ""
	}
	return fmt.Sprintf("%s/%s", b.ModelServer, b.Pod)
}
