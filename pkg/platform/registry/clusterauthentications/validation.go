/*
 * Tencent is pleased to support the open source community by making TKEStack
 * available.
 *
 * Copyright (C) 2012-2019 Tencent. All Rights Reserved.
 *
 * Licensed under the Apache License, Version 2.0 (the "License"); you may not use
 * this file except in compliance with the License. You may obtain a copy of the
 * License at
 *
 * https://opensource.org/licenses/Apache-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS, WITHOUT
 * WARRANTIES OF ANY KIND, either express or implied.  See the License for the
 * specific language governing permissions and limitations under the License.
 */

package clusterauthentications

import (
	apiMachineryValidation "k8s.io/apimachinery/pkg/api/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"tkestack.io/tke/api/platform"
)

// ValidateName is a ValidateNameFunc for names that must be a DNS
// subdomain.
var ValidateName = apiMachineryValidation.ValidateNamespaceName

// ValidateClusterAuthentication tests if required fields in the cluster are set.
func ValidateClusterAuthentication(clusterAuthentication *platform.ClusterAuthentication) field.ErrorList {
	allErrs := apiMachineryValidation.ValidateObjectMeta(&clusterAuthentication.ObjectMeta, true, ValidateName, field.NewPath("metadata"))

	if len(clusterAuthentication.ClusterName) == 0 {
		allErrs = append(allErrs, field.Required(field.NewPath("spec", "clusterName"), "must specify a cluster name"))
	}

	return allErrs
}

// ValidateClusterAuthenticationUpdate tests if required fields in the namespace set are
// set during an update.
func ValidateClusterAuthenticationUpdate(new *platform.ClusterAuthentication, old *platform.ClusterAuthentication) field.ErrorList {
	allErrs := apiMachineryValidation.ValidateObjectMetaUpdate(&new.ObjectMeta, &old.ObjectMeta, field.NewPath("metadata"))
	allErrs = append(allErrs, ValidateClusterAuthentication(new)...)

	if new.ClusterName != old.ClusterName {
		allErrs = append(allErrs, field.Invalid(field.NewPath("spec", "clusterName"), new.ClusterName, "disallowed change the cluster name"))
	}

	return allErrs
}
