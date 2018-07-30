#!/usr/bin/env bash

set -o errexit
set -o nounset
set -o pipefail

./main --v=6 --output-file-base=register --input-dirs k8s.io/api/admission/v1beta1,k8s.io/api/admissionregistration/v1alpha1,k8s.io/api/admissionregistration/v1beta1,k8s.io/api/apps/v1,k8s.io/api/apps/v1beta1,k8s.io/api/apps/v1beta2,k8s.io/api/authentication/v1,k8s.io/api/authentication/v1beta1,k8s.io/api/authorization/v1,k8s.io/api/authorization/v1beta1,k8s.io/api/autoscaling/v1,k8s.io/api/autoscaling/v2beta1,k8s.io/api/batch/v1,k8s.io/api/batch/v1beta1,k8s.io/api/batch/v2alpha1,k8s.io/api/certificates/v1beta1,k8s.io/api/coordination/v1beta1,k8s.io/api/core/v1,k8s.io/api/events/v1beta1,k8s.io/api/extensions/v1beta1,k8s.io/api/imagepolicy/v1alpha1,k8s.io/api/networking/v1,k8s.io/api/policy/v1beta1,k8s.io/api/rbac/v1,k8s.io/api/rbac/v1alpha1,k8s.io/api/rbac/v1beta1,k8s.io/api/scheduling/v1alpha1,k8s.io/api/scheduling/v1beta1,k8s.io/api/settings/v1alpha1,k8s.io/api/storage/v1,k8s.io/api/storage/v1alpha1,k8s.io/api/storage/v1beta1
