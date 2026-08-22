package main

import (
	"reflect"
	"testing"
)

func TestAppendProxyArgsNoopWhenEmpty(t *testing.T) {
	args := []string{"executor", "serve"}
	got, err := appendProxyArgs(args, nil)
	if err != nil {
		t.Fatalf("appendProxyArgs: %v", err)
	}
	if !reflect.DeepEqual(got, args) {
		t.Fatalf("got %v, want unchanged %v", got, args)
	}
}

func TestAppendProxyArgsAppendsEachAsARepeatedFlag(t *testing.T) {
	args := []string{"executor", "serve"}
	got, err := appendProxyArgs(args, []string{"vmlx=http://localhost:8005", "ollama=http://localhost:11434"})
	if err != nil {
		t.Fatalf("appendProxyArgs: %v", err)
	}
	want := []string{
		"executor", "serve",
		"--proxy", "vmlx=http://localhost:8005",
		"--proxy", "ollama=http://localhost:11434",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestAppendProxyArgsRejectsMalformedEntryBeforeInstalling(t *testing.T) {
	_, err := appendProxyArgs([]string{"executor", "serve"}, []string{"not-a-pair"})
	if err == nil {
		t.Fatal("expected an error for a proxy flag with no '=name'")
	}
}
