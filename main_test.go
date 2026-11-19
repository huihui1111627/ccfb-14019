package main

import "testing"

func TestScenarioJoinLeave(t *testing.T) {
	if rep := ScenarioJoinLeave(); len(rep.Errors) > 0 {
		t.Fatalf("%v", rep.Errors)
	}
}

func TestScenarioSourceStartStop(t *testing.T) {
	if rep := ScenarioSourceStartStop(); len(rep.Errors) > 0 {
		t.Fatalf("%v", rep.Errors)
	}
}

func TestScenarioSPTSwitch(t *testing.T) {
	if rep := ScenarioSPTSwitch(); len(rep.Errors) > 0 {
		t.Fatalf("%v", rep.Errors)
	}
}

func TestScenarioLinkFail(t *testing.T) {
	if rep := ScenarioLinkFail(); len(rep.Errors) > 0 {
		t.Fatalf("%v", rep.Errors)
	}
}

func TestScenarioAssert(t *testing.T) {
	if rep := ScenarioAssert(); len(rep.Errors) > 0 {
		t.Fatalf("%v", rep.Errors)
	}
}
