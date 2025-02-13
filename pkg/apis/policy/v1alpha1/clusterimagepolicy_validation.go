// Copyright 2022 The Sigstore Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package v1alpha1

import (
	"context"
	"fmt"
	"net/url"
	"path/filepath"
	"regexp"

	"github.com/sigstore/policy-controller/pkg/apis/glob"
	"github.com/sigstore/policy-controller/pkg/apis/policy/common"
	"github.com/sigstore/policy-controller/pkg/apis/signaturealgo"
	"github.com/sigstore/sigstore/pkg/cryptoutils"
	"k8s.io/apimachinery/pkg/util/sets"
	"knative.dev/pkg/apis"
	"knative.dev/pkg/system"

	policycontrollerconfig "github.com/sigstore/policy-controller/pkg/config"
)

var (
	// TODO: create constants in to cosign?
	validPredicateTypes = sets.NewString("custom", "slsaprovenance", "spdx", "spdxjson", "cyclonedx", "link", "vuln")

	// If a static matches, define the behaviour for it.
	validStaticRefTypes = sets.NewString("fail", "pass")

	// Valid modes for a policy
	validModes = sets.NewString("enforce", "warn")

	// ValidResourceNames for a policy match selector
	validResourceNames = sets.NewString("replicasets", "deployments", "pods", "cronjobs", "jobs", "statefulsets", "daemonsets")
)

// Validate implements apis.Validatable
func (c *ClusterImagePolicy) Validate(ctx context.Context) *apis.FieldError {
	// If we're doing status updates, do not validate the spec.
	if apis.IsInStatusUpdate(ctx) {
		return nil
	}
	return c.Spec.Validate(ctx).ViaField("spec")
}

func (spec *ClusterImagePolicySpec) Validate(ctx context.Context) (errors *apis.FieldError) {
	// Check what the configuration is and act accordingly.
	pcConfig := policycontrollerconfig.FromContextOrDefaults(ctx)

	if len(spec.Images) == 0 {
		errors = errors.Also(apis.ErrMissingField("images"))
	}
	for i, image := range spec.Images {
		errors = errors.Also(image.Validate(ctx).ViaFieldIndex("images", i))
	}
	if len(spec.Authorities) == 0 && pcConfig.FailOnEmptyAuthorities {
		errors = errors.Also(apis.ErrMissingField("authorities"))
	}
	for i, authority := range spec.Authorities {
		errors = errors.Also(authority.Validate(ctx).ViaFieldIndex("authorities", i))
	}
	if spec.Mode != "" && !validModes.Has(spec.Mode) {
		errors = errors.Also(apis.ErrInvalidValue(spec.Mode, "mode", "unsupported mode"))
	}
	for i, m := range spec.Match {
		errors = errors.Also(m.Validate(ctx).ViaFieldIndex("match", i))
	}
	// Note that we're within Spec here so that we can validate that the policy
	// FetchConfigFile is only set within Spec.Policy.
	errors = errors.Also(spec.Policy.Validate(apis.WithinSpec(ctx)))
	return
}

func (image *ImagePattern) Validate(ctx context.Context) *apis.FieldError {
	if image.Glob == "" {
		return apis.ErrMissingField("glob")
	}
	return ValidateGlob(image.Glob).ViaField("glob")
}

func (authority *Authority) Validate(ctx context.Context) *apis.FieldError {
	var errs *apis.FieldError
	if authority.Key == nil && authority.Keyless == nil && authority.Static == nil {
		errs = errs.Also(apis.ErrMissingOneOf("key", "keyless", "static"))
		// Instead of returning all the missing subfields, just return here
		// to give a more concise and arguably a more meaningful error message.
		return errs
	}
	if (authority.Key != nil && authority.Keyless != nil) ||
		(authority.Key != nil && authority.Static != nil) ||
		(authority.Keyless != nil && authority.Static != nil) {
		errs = errs.Also(apis.ErrMultipleOneOf("key", "keyless", "static"))
		// Instead of returning all the missing subfields, just return here
		// to give a more concise and arguably a more meaningful error message.
		return errs
	}

	if authority.Key != nil {
		errs = errs.Also(authority.Key.Validate(ctx).ViaField("key"))
	}
	if authority.Keyless != nil {
		errs = errs.Also(authority.Keyless.Validate(ctx).ViaField("keyless"))
	}
	if authority.Static != nil {
		errs = errs.Also(authority.Static.Validate(ctx).ViaField("static"))
		// Attestations, Sources, or CTLog do not make sense with static policy.
		if len(authority.Attestations) > 0 {
			errs = errs.Also(apis.ErrMultipleOneOf("static", "attestations"))
		}
		if len(authority.Sources) > 0 {
			errs = errs.Also(apis.ErrMultipleOneOf("static", "source"))
		}
		if authority.CTLog != nil {
			errs = errs.Also(apis.ErrMultipleOneOf("static", "ctlog"))
		}
		if authority.RFC3161Timestamp != nil {
			errs = errs.Also(apis.ErrMultipleOneOf("static", "rfc3161timestamp"))
		}
	}

	if len(authority.Sources) > 1 {
		errs = errs.Also(apis.ErrInvalidValue("source", "source", "only single source is supported"))
	} else {
		// If there are multiple sources, don't complain about each of them.
		for i, source := range authority.Sources {
			errs = errs.Also(source.Validate(ctx).ViaFieldIndex("source", i))
		}
	}

	for _, att := range authority.Attestations {
		errs = errs.Also(att.Validate(ctx).ViaField("attestations"))
	}

	return errs
}

func (s *StaticRef) Validate(ctx context.Context) *apis.FieldError {
	var errs *apis.FieldError

	if s.Action == "" {
		errs = errs.Also(apis.ErrMissingField("action"))
	} else if !validStaticRefTypes.Has(s.Action) {
		errs = errs.Also(apis.ErrInvalidValue(s.Action, "action", "unsupported action"))
	}
	return errs
}

func (matchResource *MatchResource) Validate(ctx context.Context) *apis.FieldError {
	var errs *apis.FieldError
	if matchResource.Resource != "" && !validResourceNames.Has(matchResource.Resource) {
		errs = errs.Also(apis.ErrInvalidValue(matchResource.Resource, "resource", "unsupported resource name"))
	}

	if matchResource.ResourceSelector != nil && (matchResource.Resource == "" && matchResource.Version == "" && matchResource.Group == "") {
		errs = errs.Also(apis.ErrInvalidValue(matchResource.Resource, "selector", "selector requires a resource type to match the labels"))
	}
	return errs
}

func (key *KeyRef) Validate(ctx context.Context) *apis.FieldError {
	var errs *apis.FieldError

	if key.Data == "" && key.KMS == "" && key.SecretRef == nil {
		errs = errs.Also(apis.ErrMissingOneOf("data", "kms", "secretref"))
	}

	if key.HashAlgorithm != "" {
		_, err := signaturealgo.HashAlgorithm(key.HashAlgorithm)
		if err != nil {
			errs = errs.Also(apis.ErrInvalidValue(key.HashAlgorithm, "hashAlgorithm"))
		}
	}

	if key.Data != "" {
		if key.KMS != "" || key.SecretRef != nil {
			errs = errs.Also(apis.ErrMultipleOneOf("data", "kms", "secretref"))
		}
		publicKey, err := cryptoutils.UnmarshalPEMToPublicKey([]byte(key.Data))
		if err != nil || publicKey == nil {
			errs = errs.Also(apis.ErrInvalidValue(key.Data, "data"))
		}
	} else if key.KMS != "" && key.SecretRef != nil {
		errs = errs.Also(apis.ErrMultipleOneOf("data", "kms", "secretref"))
	}
	if key.KMS != "" {
		errs = errs.Also(common.ValidateKMS(key.KMS).ViaField("kms"))
	}
	if key.SecretRef != nil && key.SecretRef.Namespace != "" && key.SecretRef.Namespace != system.Namespace() {
		errs = errs.Also(apis.ErrInvalidValue(key.SecretRef.Namespace, "secretref.namespace", "secretref.namespace is invalid. If set, it should use the same namespace where the policy-controller was deployed"))
	}
	return errs
}

func (keyless *KeylessRef) Validate(ctx context.Context) *apis.FieldError {
	var errs *apis.FieldError
	if keyless.URL == nil && keyless.CACert == nil {
		errs = errs.Also(apis.ErrMissingOneOf("url", "ca-cert"))
	}

	// TODO: Are these really mutually exclusive?
	if keyless.URL != nil && keyless.CACert != nil {
		errs = errs.Also(apis.ErrMultipleOneOf("url", "ca-cert"))
	}

	if keyless.CACert != nil {
		errs = errs.Also(keyless.DeepCopy().CACert.Validate(ctx).ViaField("ca-cert"))
	}
	// Check that identities is specified.
	if len(keyless.Identities) == 0 {
		errs = errs.Also(apis.ErrMissingField("identities"))
	}
	for i, identity := range keyless.Identities {
		errs = errs.Also(identity.Validate(ctx).ViaFieldIndex("identities", i))
	}
	return errs
}

func (source *Source) Validate(ctx context.Context) *apis.FieldError {
	var errs *apis.FieldError
	if source.OCI != "" {
		if err := common.ValidateOCI(source.OCI); err != nil {
			errs = errs.Also(apis.ErrInvalidValue(source.OCI, "oci", err.Error()))
		}
	}

	if len(source.SignaturePullSecrets) > 0 {
		for i, secret := range source.SignaturePullSecrets {
			if secret.Name == "" {
				errs = errs.Also(apis.ErrMissingField("name")).ViaFieldIndex("signaturePullSecrets", i)
			}
		}
	}
	return errs
}

func (a *Attestation) Validate(ctx context.Context) *apis.FieldError {
	var errs *apis.FieldError
	if a.Name == "" {
		errs = errs.Also(apis.ErrMissingField("name"))
	}
	if a.PredicateType == "" {
		errs = errs.Also(apis.ErrMissingField("predicateType"))
	} else if !validPredicateTypes.Has(a.PredicateType) {
		// This could be a fully specified URL, so check for that here.
		if _, err := url.ParseRequestURI(a.PredicateType); err != nil {
			errs = errs.Also(apis.ErrInvalidValue(a.PredicateType, "predicateType", "unsupported predicate type"))
		}
	}
	errs = errs.Also(a.Policy.Validate(ctx).ViaField("policy"))
	return errs
}

func (cmr *ConfigMapReference) Validate(ctx context.Context) *apis.FieldError {
	var errs *apis.FieldError
	if cmr.Name == "" {
		errs = errs.Also(apis.ErrMissingField("name"))
	}
	if cmr.Key == "" {
		errs = errs.Also(apis.ErrMissingField("key"))
	}
	return errs
}

func (r *RemotePolicy) Validate(ctx context.Context) *apis.FieldError {
	var errs *apis.FieldError
	urlObj := r.URL
	u, err := url.Parse(urlObj.String())
	if err != nil || (err == nil && (u.Host == "" || u.Scheme == "" || u.Scheme != "https")) {
		errs = errs.Also(apis.ErrInvalidValue(r.URL.String(), "url", "url valid is invalid. host and https scheme are expected"))
	}
	if r.Sha256sum == "" {
		errs = errs.Also(apis.ErrMissingField("sha256sum"))
	}
	return errs
}

func (p *Policy) Validate(ctx context.Context) *apis.FieldError {
	if p == nil {
		return nil
	}
	var errs *apis.FieldError
	if p.Type != "cue" && p.Type != "rego" {
		errs = errs.Also(apis.ErrInvalidValue(p.Type, "type", "only [cue,rego] are supported at the moment"))
	}
	if p.Data == "" && p.ConfigMapRef == nil && p.Remote == nil {
		errs = errs.Also(apis.ErrMissingField("data", "configMapRef", "remote"))
	}
	if p.Data != "" && p.ConfigMapRef != nil && p.Remote != nil {
		errs = errs.Also(apis.ErrMultipleOneOf("data", "configMapRef", "remote"))
	}
	if (p.Data != "" && p.ConfigMapRef != nil) ||
		(p.Data != "" && p.Remote != nil) ||
		(p.ConfigMapRef != nil && p.Remote != nil) {
		errs = errs.Also(apis.ErrMultipleOneOf("data", "configMapRef", "remote"))
	}
	if p.Remote != nil {
		errs = errs.Also(p.Remote.Validate(ctx).ViaField("remote"))
	}
	if p.ConfigMapRef != nil {
		errs = errs.Also(p.ConfigMapRef.Validate(ctx).ViaField("configMapRef"))
	}
	if !apis.IsInSpec(ctx) && p.FetchConfigFile != nil {
		errs = errs.Also(apis.ErrDisallowedFields("fetchConfigFile"))
	}
	if !apis.IsInSpec(ctx) && p.IncludeSpec != nil {
		errs = errs.Also(apis.ErrDisallowedFields("includeSpec"))
	}
	if !apis.IsInSpec(ctx) && p.IncludeObjectMeta != nil {
		errs = errs.Also(apis.ErrDisallowedFields("includeObjectMeta"))
	}
	if !apis.IsInSpec(ctx) && p.IncludeTypeMeta != nil {
		errs = errs.Also(apis.ErrDisallowedFields("includeTypeMeta"))
	}
	// TODO(vaikas): How to validate the cue / rego bytes here (data).
	return errs
}

func (identity *Identity) Validate(ctx context.Context) *apis.FieldError {
	var errs *apis.FieldError
	if identity.Issuer != "" && identity.IssuerRegExp != "" {
		errs = errs.Also(apis.ErrMultipleOneOf("issuer", "issuerRegExp"))
	}
	if identity.Subject != "" && identity.SubjectRegExp != "" {
		errs = errs.Also(apis.ErrMultipleOneOf("subject", "subjectRegExp"))
	}
	if identity.IssuerRegExp != "" {
		errs = errs.Also(ValidateRegex(identity.IssuerRegExp).ViaField("issuerRegExp"))
	}
	if identity.SubjectRegExp != "" {
		errs = errs.Also(ValidateRegex(identity.SubjectRegExp).ViaField("subjectRegExp"))
	}
	if identity.SubjectRegExp == "" && identity.Subject == "" {
		errs = errs.Also(apis.ErrMissingField("subject", "subjectRegExp"))
	}
	if identity.IssuerRegExp == "" && identity.Issuer == "" {
		errs = errs.Also(apis.ErrMissingField("issuer", "issuerRegExp"))
	}
	return errs
}

// ValidateGlob glob compilation by testing against empty string
func ValidateGlob(g string) *apis.FieldError {
	if _, err := filepath.Match(g, ""); err != nil {
		return apis.ErrInvalidValue(g, apis.CurrentField, fmt.Sprintf("glob is invalid: %v", err))
	}
	if _, err := glob.Compile(g); err != nil {
		return apis.ErrInvalidValue(g, apis.CurrentField, fmt.Sprintf("glob is invalid: %v", err))
	}
	return nil
}

func ValidateRegex(regex string) *apis.FieldError {
	_, err := regexp.Compile(regex)
	if err != nil {
		return apis.ErrInvalidValue(regex, apis.CurrentField, fmt.Sprintf("regex is invalid: %v", err))
	}

	return nil
}
