package kube

import (
	"fmt"

	resourceapi "k8s.io/api/resource/v1"

	"github.com/anchor-dra/anchor/internal/constants"
	"github.com/anchor-dra/anchor/internal/model"
)

func AllocationParameters(claim *resourceapi.ResourceClaim) (model.DeviceClassParameters, model.ClaimParameters, error) {
	var class model.DeviceClassParameters
	var endpoint model.ClaimParameters
	if claim.Status.Allocation == nil {
		return class, endpoint, fmt.Errorf("ResourceClaim has no allocation")
	}
	for _, config := range claim.Status.Allocation.Devices.Config {
		if config.Opaque == nil || config.Opaque.Driver != constants.DriverName {
			continue
		}
		switch config.Source {
		case resourceapi.AllocationConfigSourceClass:
			value, err := model.Decode[model.DeviceClassParameters](config.Opaque.Parameters.Raw)
			if err != nil {
				return class, endpoint, err
			}
			class = value
		case resourceapi.AllocationConfigSourceClaim:
			value, err := model.Decode[model.ClaimParameters](config.Opaque.Parameters.Raw)
			if err != nil {
				return class, endpoint, err
			}
			endpoint = value
		}
	}
	if err := class.NormalizeAndValidate(); err != nil {
		return class, endpoint, fmt.Errorf("invalid DeviceClass parameters: %w", err)
	}
	if len(endpoint.Addresses) == 0 {
		return class, endpoint, fmt.Errorf("claim contains no %s address configuration", constants.DriverName)
	}
	return class, endpoint, nil
}
