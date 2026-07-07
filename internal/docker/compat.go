package docker

import "encoding/json"

func convertDockerType[T any](src any) (T, error) {
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

func convertDockerPointer[T any](src any) (*T, error) {
	if src == nil {
		return nil, nil
	}
	dst, err := convertDockerType[T](src)
	if err != nil {
		return nil, err
	}
	return &dst, nil
}
