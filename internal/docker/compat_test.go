package docker

import "testing"

type compatibilitySource struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

type compatibilityTarget struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

func TestConvertDockerTypeAndPointer(t *testing.T) {
	source := compatibilitySource{Name: "demo", Count: 3}
	converted, err := ConvertDockerType[compatibilityTarget](source)
	if err != nil {
		t.Fatal(err)
	}
	if converted != (compatibilityTarget{Name: "demo", Count: 3}) {
		t.Fatalf("ConvertDockerType() = %#v", converted)
	}

	pointer, err := ConvertDockerPointer[compatibilityTarget](&source)
	if err != nil {
		t.Fatal(err)
	}
	if pointer == nil || *pointer != converted {
		t.Fatalf("ConvertDockerPointer() = %#v", pointer)
	}
	if pointer, err := ConvertDockerPointer[compatibilityTarget](nil); err != nil || pointer != nil {
		t.Fatalf("ConvertDockerPointer(nil) = %#v, %v", pointer, err)
	}
}

func TestConvertDockerTypeReportsCodecErrors(t *testing.T) {
	if _, err := ConvertDockerType[compatibilityTarget](func() {}); err == nil {
		t.Fatal("ConvertDockerType(function) error = nil")
	}
	if _, err := ConvertDockerType[chan int](map[string]int{"value": 1}); err == nil {
		t.Fatal("ConvertDockerType(channel target) error = nil")
	}
}
