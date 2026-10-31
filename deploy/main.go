// deploy runs the Terraform configuration from Go.
//
//	go run . validate     check the configuration, needs no Azure login
//	go run . plan         write the changes to a plan file and show them
//	go run . apply        apply exactly that plan file, nothing else
//
// terraform-exec does not embed Terraform, it starts the terraform binary and gives us typed results.
// Any language can do the same by starting the process. Go just has an official library for it.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/hashicorp/terraform-exec/tfexec"
)

const planFile = "tfplan"

func main() {
	dir := flag.String("dir", "../infra/azure", "directory of the Terraform configuration")
	flag.Parse()
	if flag.NArg() != 1 {
		log.Fatal("usage: deploy [-dir path] validate|plan|apply")
	}
	if err := run(context.Background(), *dir, flag.Arg(0)); err != nil {
		log.Fatal(err)
	}
}

func run(ctx context.Context, dir, command string) error {
	binary, err := exec.LookPath("terraform")
	if err != nil {
		return fmt.Errorf("terraform is not installed: %w", err)
	}
	tf, err := tfexec.NewTerraform(dir, binary)
	if err != nil {
		return err
	}
	tf.SetStdout(os.Stdout)
	tf.SetStderr(os.Stderr)

	switch command {
	case "validate":
		return validate(ctx, tf)
	case "plan":
		return plan(ctx, tf)
	case "apply":
		return apply(ctx, tf)
	default:
		return fmt.Errorf("unknown command %q", command)
	}
}

func validate(ctx context.Context, tf *tfexec.Terraform) error {
	if err := tf.Init(ctx, tfexec.Backend(false)); err != nil {
		return err
	}
	result, err := tf.Validate(ctx)
	if err != nil {
		return err
	}
	for _, d := range result.Diagnostics {
		fmt.Printf("%s: %s: %s\n", d.Severity, d.Summary, d.Detail)
	}
	if !result.Valid {
		return errors.New("configuration is invalid")
	}
	fmt.Println("configuration is valid")
	return nil
}

func plan(ctx context.Context, tf *tfexec.Terraform) error {
	if err := tf.Init(ctx); err != nil {
		return err
	}
	changes, err := tf.Plan(ctx, tfexec.Out(planFile))
	if err != nil {
		return err
	}
	if !changes {
		fmt.Println("no changes")
		return os.Remove(planPath(tf))
	}
	fmt.Println("review the plan above, then run: deploy apply")
	return nil
}

// apply only takes the saved plan, so what runs is exactly what was reviewed.
func apply(ctx context.Context, tf *tfexec.Terraform) error {
	if _, err := os.Stat(planPath(tf)); err != nil {
		return fmt.Errorf("no saved plan, run plan first: %w", err)
	}
	if err := tf.Apply(ctx, tfexec.DirOrPlan(planFile)); err != nil {
		return err
	}
	return os.Remove(planPath(tf))
}

func planPath(tf *tfexec.Terraform) string {
	return filepath.Join(tf.WorkingDir(), planFile)
}
