/*
Copyright 2021 Gravitational, Inc.

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

package services

import (
	"fmt"

	"github.com/gravitational/teleport/api/types"
	"github.com/gravitational/teleport/lib/utils"
	"github.com/gravitational/trace"
)

// ClusterAuditConfigSpecFromObject returns audit config spec from object.
func ClusterAuditConfigSpecFromObject(in interface{}) (*types.ClusterAuditConfigSpecV2, error) {
	var cfg types.ClusterAuditConfigSpecV2
	if in == nil {
		return &cfg, nil
	}
	if err := utils.ObjectToStruct(in, &cfg); err != nil {
		return nil, trace.Wrap(err)
	}
	return &cfg, nil
}

// ClusterAuditConfigSpecSchema is JSON schema for ClusterAuditConfig spec.
const ClusterAuditConfigSpecSchema = `{
	"type": "object",
	"additionalProperties": false,
	"properties": {
		"type": {"type": "string"},
		"region": {"type": "string"},
		"audit_events_uri": {
			"anyOf": [
				{"type": "string"},
				{"type": "array", "items": {"type": "string"}}
			]
		},
		"audit_sessions_uri": {"type": "string"},
		"audit_table_name": {"type": "string"}
	}
}`

// GetClusterAuditConfigSchema returns full ClusterAuditConfig JSON schema.
func GetClusterAuditConfigSchema(extensionSchema string) string {
	return fmt.Sprintf(V2SchemaTemplate, MetadataSchema, ClusterAuditConfigSpecSchema, DefaultDefinitions)
}

// UnmarshalClusterAuditConfig unmarshals the ClusterAuditConfig resource from JSON.
func UnmarshalClusterAuditConfig(bytes []byte, opts ...MarshalOption) (types.ClusterAuditConfig, error) {
	var auditConfig types.ClusterAuditConfigV2

	if len(bytes) == 0 {
		return nil, trace.BadParameter("missing resource data")
	}

	cfg, err := CollectOptions(opts)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	if cfg.SkipValidation {
		if err := utils.FastUnmarshal(bytes, &auditConfig); err != nil {
			return nil, trace.BadParameter(err.Error())
		}
	} else {
		err = utils.UnmarshalWithSchema(GetClusterAuditConfigSchema(""), &auditConfig, bytes)
		if err != nil {
			return nil, trace.BadParameter(err.Error())
		}
	}

	err = auditConfig.CheckAndSetDefaults()
	if err != nil {
		return nil, trace.Wrap(err)
	}

	if cfg.ID != 0 {
		auditConfig.SetResourceID(cfg.ID)
	}
	if !cfg.Expires.IsZero() {
		auditConfig.SetExpiry(cfg.Expires)
	}
	return &auditConfig, nil
}

// MarshalClusterAuditConfig marshals the ClusterAuditConfig resource to JSON.
func MarshalClusterAuditConfig(auditConfig types.ClusterAuditConfig, opts ...MarshalOption) ([]byte, error) {
	if err := auditConfig.CheckAndSetDefaults(); err != nil {
		return nil, trace.Wrap(err)
	}

	cfg, err := CollectOptions(opts)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	switch auditConfig := auditConfig.(type) {
	case *types.ClusterAuditConfigV2:
		if version := auditConfig.GetVersion(); version != V2 {
			return nil, trace.BadParameter("mismatched session recording config version %v and type %T", version, auditConfig)
		}
		if !cfg.PreserveResourceID {
			// avoid modifying the original object
			// to prevent unexpected data races
			copy := *auditConfig
			copy.SetResourceID(0)
			auditConfig = &copy
		}
		return utils.FastMarshal(auditConfig)
	default:
		return nil, trace.BadParameter("unrecognized session recording config version %T", auditConfig)
	}
}
