package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

// GroupVersion is the group/version for our CRD.
var (
	GroupVersion  = schema.GroupVersion{Group: "telemetry.stackit.local", Version: "v1alpha1"}
	SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}
	AddToScheme   = SchemeBuilder.AddToScheme
)

func init() {
	SchemeBuilder.Register(&TelemetryRouter{}, &TelemetryRouterList{})
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Replicas",type=integer,JSONPath=`.spec.replicas`

// TelemetryRouter is the Schema for telemetryrouters.
type TelemetryRouter struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              TelemetryRouterSpec   `json:"spec,omitempty"`
	Status            TelemetryRouterStatus `json:"status,omitempty"`
}

type TelemetryRouterSpec struct {
	// +kubebuilder:default=1
	Replicas     int32          `json:"replicas,omitempty"`
	Sources      []string       `json:"sources,omitempty"`
	Transforms   []VRLTransform `json:"transforms,omitempty"`
	Destinations []Destination  `json:"destinations"`
}

type VRLTransform struct {
	Name string `json:"name"`
	VRL  string `json:"vrl"`
}

type Destination struct {
	Name     string `json:"name"`
	Type     string `json:"type"` // OTLP | LOKI | PROMETHEUS
	Endpoint string `json:"endpoint"`
}

type TelemetryRouterStatus struct {
	Phase            string `json:"phase,omitempty"`
	VectorConfigHash string `json:"vectorConfigHash,omitempty"`
	ReadyReplicas    int32  `json:"readyReplicas,omitempty"`
	Message          string `json:"message,omitempty"`
	LastReconcileAt  string `json:"lastReconcileAt,omitempty"`
}

// +kubebuilder:object:root=true
type TelemetryRouterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []TelemetryRouter `json:"items"`
}

// DeepCopyObject implements runtime.Object.
func (in *TelemetryRouter) DeepCopyObject() runtime.Object {
	out := &TelemetryRouter{}
	in.DeepCopyInto(out)
	return out
}
func (in *TelemetryRouter) DeepCopyInto(out *TelemetryRouter) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	in.Spec.DeepCopyInto(&out.Spec)
	out.Status = in.Status
}
func (in *TelemetryRouterSpec) DeepCopyInto(out *TelemetryRouterSpec) {
	*out = *in
	if in.Sources != nil {
		out.Sources = make([]string, len(in.Sources))
		copy(out.Sources, in.Sources)
	}
	if in.Transforms != nil {
		out.Transforms = make([]VRLTransform, len(in.Transforms))
		copy(out.Transforms, in.Transforms)
	}
	if in.Destinations != nil {
		out.Destinations = make([]Destination, len(in.Destinations))
		copy(out.Destinations, in.Destinations)
	}
}
func (in *TelemetryRouterList) DeepCopyObject() runtime.Object {
	out := &TelemetryRouterList{}
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		out.Items = make([]TelemetryRouter, len(in.Items))
		for i := range in.Items {
			in.Items[i].DeepCopyInto(&out.Items[i])
		}
	}
	return out
}
