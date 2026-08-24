package docker

import "encoding/json"

// ConvertDockerType bridges equivalent structs exposed by different Moby API
// packages while the callers are migrated independently.
func ConvertDockerType[T any](src any) (T, error) {
	var dst T
	data, err := json.Marshal(src)
	if err != nil {
		return dst, err
	}
	if err := json.Unmarshal(data, &dst); err != nil {
		return dst, err
	}
	return dst, nil
}

// ConvertDockerPointer is the pointer-preserving form of ConvertDockerType.
func ConvertDockerPointer[T any](src any) (*T, error) {
	if src == nil {
		return nil, nil
	}
	dst, err := ConvertDockerType[T](src)
	if err != nil {
		return nil, err
	}
	return &dst, nil
}
