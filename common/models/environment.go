package models

import swagger "github.com/zpatrick/go-plugin-swagger"

type Environment struct {
	EnvironmentID   string   `json:"environment_id"`
	EnvironmentName string   `json:"environment_name"`
	ClusterCount    int      `json:"cluster_count"`
	InstanceSize    string   `json:"instance_size"`
	SecurityGroupID string   `json:"security_group_id"`
	OperatingSystem string   `json:"operating_system"`
	AMIID           string   `json:"ami_id"`
	Links           []string `json:"links"`
}

func (e Environment) Definition() swagger.Definition {
	return swagger.Definition{
		Type: "object",
		Properties: map[string]swagger.Property{
			"environment_id":    swagger.NewStringProperty(),
			"environment_name":  swagger.NewStringProperty(),
			"cluster_count":     swagger.NewIntProperty(),
			"instance_size":     swagger.NewStringProperty(),
			"security_group_id": swagger.NewStringProperty(),
			"operating_system":  swagger.NewStringProperty(),
			"ami_id":            swagger.NewStringProperty(),
			"links":             swagger.NewStringSliceProperty(),
		},
	}
}
