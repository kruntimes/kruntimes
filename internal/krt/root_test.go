package krt

import "testing"

func TestRootCommandExposesKlogVerbosityFlag(t *testing.T) {
	command := NewRootCmd()
	flag := command.PersistentFlags().Lookup("v")
	if flag == nil {
		t.Fatal("krt root command does not expose klog --v")
	}
	if flag.DefValue != "0" {
		t.Fatalf("klog --v default = %q, want 0", flag.DefValue)
	}
}
