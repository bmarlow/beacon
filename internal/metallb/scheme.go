/*
Copyright 2026.

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

package metallb

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// AddToScheme registers the minimal MetalLB types with a runtime.Scheme so the
// controller-runtime client can read and write them.
func AddToScheme(s *runtime.Scheme) error {
	s.AddKnownTypeWithName(IPAddressPoolGVK, &IPAddressPool{})
	s.AddKnownTypeWithName(IPAddressPoolListGVK, &IPAddressPoolList{})
	s.AddKnownTypeWithName(BGPAdvertisementGVK, &BGPAdvertisement{})
	s.AddKnownTypeWithName(BGPAdvertisementListGVK, &BGPAdvertisementList{})
	metav1.AddToGroupVersion(s, GroupVersion)
	return nil
}

// --- Minimal DeepCopy implementations (client.Object requires them) ---

func (in *IPAddressPool) DeepCopyInto(out *IPAddressPool) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	if in.Spec.Addresses != nil {
		out.Spec.Addresses = make([]string, len(in.Spec.Addresses))
		copy(out.Spec.Addresses, in.Spec.Addresses)
	}
	if in.Spec.AutoAssign != nil {
		v := *in.Spec.AutoAssign
		out.Spec.AutoAssign = &v
	}
}

func (in *IPAddressPool) DeepCopy() *IPAddressPool {
	if in == nil {
		return nil
	}
	out := new(IPAddressPool)
	in.DeepCopyInto(out)
	return out
}

func (in *IPAddressPool) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

func (in *IPAddressPoolList) DeepCopyInto(out *IPAddressPoolList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		out.Items = make([]IPAddressPool, len(in.Items))
		for i := range in.Items {
			in.Items[i].DeepCopyInto(&out.Items[i])
		}
	}
}

func (in *IPAddressPoolList) DeepCopy() *IPAddressPoolList {
	if in == nil {
		return nil
	}
	out := new(IPAddressPoolList)
	in.DeepCopyInto(out)
	return out
}

func (in *IPAddressPoolList) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

func (in *BGPAdvertisement) DeepCopyInto(out *BGPAdvertisement) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	in.Spec.DeepCopyInto(&out.Spec)
}

func (in *BGPAdvertisementSpec) DeepCopyInto(out *BGPAdvertisementSpec) {
	*out = *in
	if in.IPAddressPools != nil {
		out.IPAddressPools = make([]string, len(in.IPAddressPools))
		copy(out.IPAddressPools, in.IPAddressPools)
	}
	if in.IPAddressPoolSelectors != nil {
		out.IPAddressPoolSelectors = make([]metav1.LabelSelector, len(in.IPAddressPoolSelectors))
		for i := range in.IPAddressPoolSelectors {
			in.IPAddressPoolSelectors[i].DeepCopyInto(&out.IPAddressPoolSelectors[i])
		}
	}
	if in.ServiceSelectors != nil {
		out.ServiceSelectors = make([]metav1.LabelSelector, len(in.ServiceSelectors))
		for i := range in.ServiceSelectors {
			in.ServiceSelectors[i].DeepCopyInto(&out.ServiceSelectors[i])
		}
	}
	if in.AggregationLength != nil {
		v := *in.AggregationLength
		out.AggregationLength = &v
	}
	if in.Communities != nil {
		out.Communities = make([]string, len(in.Communities))
		copy(out.Communities, in.Communities)
	}
}

func (in *BGPAdvertisement) DeepCopy() *BGPAdvertisement {
	if in == nil {
		return nil
	}
	out := new(BGPAdvertisement)
	in.DeepCopyInto(out)
	return out
}

func (in *BGPAdvertisement) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

func (in *BGPAdvertisementList) DeepCopyInto(out *BGPAdvertisementList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		out.Items = make([]BGPAdvertisement, len(in.Items))
		for i := range in.Items {
			in.Items[i].DeepCopyInto(&out.Items[i])
		}
	}
}

func (in *BGPAdvertisementList) DeepCopy() *BGPAdvertisementList {
	if in == nil {
		return nil
	}
	out := new(BGPAdvertisementList)
	in.DeepCopyInto(out)
	return out
}

func (in *BGPAdvertisementList) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}
