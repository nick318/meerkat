package main

import "testing"

func TestATagFromANewerBuildReadsAsUntagged(t *testing.T) {
	prod := " Prod "
	if env, ok := parseEnvOpt(&prod); !ok || env != EnvProd {
		t.Fatalf("parseEnvOpt(%q) = %v, %v", prod, env, ok)
	}
	qa := "qa"
	if env, ok := parseEnvOpt(&qa); ok {
		t.Fatalf("parseEnvOpt(%q) = %v", qa, env)
	}
	if env, ok := parseEnvOpt(nil); ok {
		t.Fatalf("parseEnvOpt(nil) = %v", env)
	}
}
