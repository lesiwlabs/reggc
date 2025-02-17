// op runs operations for this service.
package main

import (
	_ "embed"
	"os"

	"labs.lesiw.io/ops/goapp"
	k8sapp "labs.lesiw.io/ops/k8s/goapp"
	"lesiw.io/ops"
)

// Ops represents the operations for this service.
type Ops struct{ k8sapp.Ops }

//go:embed role.yml
var role string

func main() {
	if len(os.Args) < 2 {
		os.Args = append(os.Args, "build")
	}
	goapp.Name = "reggc"
	o := Ops{}
	o.K8sDefinitions = role
	o.ServiceAccount = "reggc"
	ops.Handle(o)
}
