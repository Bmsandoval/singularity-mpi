// Copyright (c) 2019, Sylabs Inc. All rights reserved.
// This software is licensed under a 3-clause BSD license. Please consult the
// LICENSE.md file distributed with the sources of this project regarding your
// rights to use or distribute this software.

package container

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/gvallee/go_util/pkg/util"
	"github.com/sylabs/singularity-mpi/internal/pkg/implem"
	"github.com/sylabs/singularity-mpi/pkg/buildenv"
	"github.com/sylabs/singularity-mpi/pkg/checker"
	"github.com/sylabs/singularity-mpi/pkg/sy"
	"github.com/sylabs/singularity-mpi/pkg/syexec"
	"github.com/sylabs/singularity-mpi/pkg/sys"
)

const (
	// KeyPassphrase is the name of the environment variable used to specify the passphrase of the key to be used to sign images
	KeyPassphrase = "SY_KEY_PASSPHRASE"

	// KeyIndex is the index of the key to use to sign images
	KeyIndexEnvVar = "SY_KEY_INDEX"

	// HybridModel is the identifier used to identify the hybrid model
	HybridModel = "hybrid"

	// BindModel is the identifier used to identify the bind-mount model
	BindModel = "bind"

	// defaultExecArgs
	defaultExecArgs = "--no-home"
)

// Config is a structure representing a container
type Config struct {
	// Name of the container
	Name string

	// Path to the container's image
	Path string

	// BuildDir is the path to the directory from where the image must be built
	BuildDir string

	// InstallDir is the directory where the container needs to be stored
	InstallDir string

	// DefFile is the path to the definition file associated to the container
	DefFile string

	// Distro is the ID of the Linux distribution to use in the container
	Distro string

	// URL is the URL of the container image to use when pulling the image from a registry
	URL string

	// Model specifies the model to follow for MPI inside the container
	Model string

	// AppExe is the command to start the application in the container
	AppExe string

	// MPIDir is the directory in the container where MPI is supposed to be installed or mounted
	MPIDir string

	// Binds is the set of bind options to use while starting the container
	Binds []string
}

// Create builds a container based on a MPI configuration
func Create(container *Config, sysCfg *sys.Config) error {
	var err error

	// Some sanity checks
	if container.BuildDir == "" {
		return fmt.Errorf("build directory is undefined")
	}

	if sysCfg.SingularityBin == "" {
		sysCfg.SingularityBin, err = exec.LookPath("singularity")
		if err != nil {
			return fmt.Errorf("singularity not available: %s", err)
		}
	}

	// Check integrity of the installation of Singularity
	err = sy.CheckIntegrity(sysCfg)
	if err != nil {
		return fmt.Errorf("Singularity installation has been compromised: %s", err)
	}

	// Prepare the configuration of the container
	if container.Name == "" {
		container.Name = "singularity_mpi.sif"
	}

	if container.Path == "" {
		container.Path = filepath.Join(container.InstallDir, container.Name)
	}

	log.Printf("- Creating image %s...", container.Path)

	// The definition file is ready so we simple build the container using the Singularity command
	if sysCfg.Debug {
		err = checker.CheckDefFile(container.DefFile)
		if err != nil {
			return fmt.Errorf("unable to check definition file: %s", err)
		}
	}

	log.Printf("-> Using definition file %s", container.DefFile)

	var cmd syexec.SyCmd
	singularityVersion := sy.GetVersion(sysCfg)
	cmd.ManifestName = "build"
	cmd.ManifestData = []string{"Singularity version: " + singularityVersion}
	cmd.ManifestDir = container.InstallDir
	cmd.ManifestFileHash = []string{container.DefFile, container.Path}
	cmd.ExecDir = container.BuildDir
	if sysCfg.Nopriv {
		cmd.BinPath = sysCfg.SingularityBin
		cmd.CmdArgs = []string{"build", "--fakeroot", container.Path, container.DefFile}
	} else if sy.IsSudoCmd("build", sysCfg) {
		cmd.BinPath = sysCfg.SudoBin
		cmd.ManifestFileHash = append(cmd.ManifestFileHash, sysCfg.SingularityBin)
		cmd.CmdArgs = []string{sysCfg.SingularityBin, "build", container.Path, container.DefFile}
	} else {
		cmd.BinPath = sysCfg.SingularityBin
		cmd.CmdArgs = []string{"build", container.Path, container.DefFile}
	}
	res := cmd.Run()
	if res.Err != nil {
		return fmt.Errorf("failed to execute command - stdout: %s; stderr: %s; err: %s", res.Stdout, res.Stderr, res.Err)
	}

	// We make all SIF file executable to make it easier to integrate with other tools
	// such as PRRTE.
	f, err := os.Open(container.Path)
	if err != nil {
		return fmt.Errorf("failed to open %s", container.Path)
	}
	defer f.Close()
	err = f.Chmod(0755)
	if err != nil {
		return fmt.Errorf("failed to change %s mode", container.Path)
	}

	return nil
}

// PullContainerImage pulls from a registry the appropriate image
func PullContainerImage(cfg *Config, mpiImplm *implem.Info, sysCfg *sys.Config, syConfig *sy.MPIToolConfig) error {
	// Sanity checks
	if cfg.URL == "" {
		return fmt.Errorf("undefined image URL")
	}

	if sysCfg.SingularityBin == "" {
		var err error
		sysCfg.SingularityBin, err = exec.LookPath("singularity")
		if err != nil {
			return fmt.Errorf("failed to find Singularity binary: %s", err)
		}
	}

	// Check integrity of the installation of Singularity
	err := sy.CheckIntegrity(sysCfg)
	if err != nil {
		return fmt.Errorf("Singularity installation has been compromised: %s", err)
	}

	log.Println("* Pulling container with the following MPI configuration *")
	log.Println("-> Build container in", cfg.BuildDir)
	log.Println("-> MPI implementation:", mpiImplm.ID)
	log.Println("-> MPI version:", mpiImplm.Version)
	log.Println("-> Image URL:", cfg.URL)

	err = Pull(cfg, sysCfg)
	if err != nil {
		return fmt.Errorf("failed to pull image: %s", err)
	}

	return nil
}

// Pull retieves an image from the registry
func Pull(containerInfo *Config, sysCfg *sys.Config) error {
	var stdout, stderr bytes.Buffer

	log.Printf("* Singularity binary: %s\n", sysCfg.SingularityBin)
	log.Printf("* Container path: %s\n", containerInfo.Path)
	log.Printf("* Image URL: %s\n", containerInfo.URL)
	log.Printf("* Build directory: %s\n", containerInfo.BuildDir)
	log.Printf("-> Pulling image: %s pull %s %s", sysCfg.SingularityBin, containerInfo.Path, containerInfo.URL)

	if sysCfg.SingularityBin == "" || containerInfo.Path == "" || containerInfo.URL == "" || containerInfo.BuildDir == "" {
		return fmt.Errorf("invalid parameter(s)")
	}

	// Check integrity of the installation of Singularity
	err := sy.CheckIntegrity(sysCfg)
	if err != nil {
		return fmt.Errorf("Singularity installation has been compromised: %s", err)
	}

	if sysCfg.Persistent != "" && util.PathExists(containerInfo.Path) {
		log.Printf("* Persistent mode, %s already available, skipping...", containerInfo.Path)
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), sys.CmdTimeout*2*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(ctx, sysCfg.SingularityBin, "pull", containerInfo.Path, containerInfo.URL)
	cmd.Dir = containerInfo.BuildDir
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err = cmd.Run()
	if err != nil {
		return fmt.Errorf("failed to execute command - stdout: %s; stderr: %s; err: %s", stdout.String(), stderr.String(), err)
	}

	return nil
}

// Sign signs a given image
func Sign(container *Config, sysCfg *sys.Config) error {
	var stdout, stderr bytes.Buffer

	// Check integrity of the installation of Singularity
	err := sy.CheckIntegrity(sysCfg)
	if err != nil {
		return fmt.Errorf("Singularity installation has been compromised: %s", err)
	}

	log.Printf("-> Signing container (%s)", container.Path)
	ctx, cancel := context.WithTimeout(context.Background(), sys.CmdTimeout*2*time.Minute)
	defer cancel()

	indexIdx := "0"
	if os.Getenv(KeyIndexEnvVar) != "" {
		indexIdx = os.Getenv(KeyIndexEnvVar)
	}

	var cmd *exec.Cmd
	if sy.IsSudoCmd("sign", sysCfg) {
		cmd = exec.CommandContext(ctx, sysCfg.SudoBin, sysCfg.SingularityBin, "sign", "--keyidx", indexIdx, container.Path)
	} else {
		cmd = exec.CommandContext(ctx, sysCfg.SingularityBin, "sign", "--keyidx", indexIdx, container.Path)
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		log.Fatal(err)
	}

	go func() {
		defer stdin.Close()
		passphrase := os.Getenv(KeyPassphrase)
		_, err := io.WriteString(stdin, passphrase)
		if err != nil {
			log.Fatal(err)
		}
	}()
	cmd.Dir = container.BuildDir
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err = cmd.Run()
	if err != nil {
		return fmt.Errorf("failed to execute command - stdout: %s; stderr: %s; err: %s", stdout.String(), stderr.String(), err)
	}

	return nil
}

// Upload uploads an image to a registry
func Upload(containerInfo *Config, sysCfg *sys.Config) error {
	var stdout, stderr bytes.Buffer

	err := sy.CheckIntegrity(sysCfg)
	if err != nil {
		return fmt.Errorf("Singularity installation has been compromised: %s", err)
	}

	log.Printf("-> Uploading container %s to %s", containerInfo.Path, sysCfg.Registry)
	ctx, cancel := context.WithTimeout(context.Background(), sys.CmdTimeout*2*time.Minute)
	defer cancel()

	var cmd *exec.Cmd
	if sy.IsSudoCmd("push", sysCfg) {
		cmd = exec.CommandContext(ctx, sysCfg.SudoBin, sysCfg.SingularityBin, "push", containerInfo.Path, sysCfg.Registry)
	} else {
		cmd = exec.CommandContext(ctx, sysCfg.SingularityBin, "push", containerInfo.Path, sysCfg.Registry)
	}
	cmd.Dir = containerInfo.BuildDir
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err = cmd.Run()
	if err != nil {
		return fmt.Errorf("failed to execute command - stdout: %s; stderr: %s; err: %s", stdout.String(), stderr.String(), err)
	}

	return nil
}

// GetContainerDefaultName returns the default name for any container based on the configuration details
func GetContainerDefaultName(distro string, mpiID string, mpiVersion string, appName string, model string) string {
	return strings.Replace(distro, ":", "-", -1) + "-" + mpiID + "-" + mpiVersion + "-" + appName + "-" + model
}

func parseInspectOutput(output string) (Config, implem.Info) {
	var cfg Config
	var mpiCfg implem.Info

	lines := strings.Split(output, "\n")
	for _, line := range lines {
		if strings.Contains(line, "MPI_Implementation: ") {
			mpiCfg.ID = strings.Replace(line, "MPI_Implementation: ", "", -1)
		}
		if strings.Contains(line, "MPI_Version: ") {
			mpiCfg.Version = strings.Replace(line, "MPI_Version: ", "", -1)
		}
		if strings.Contains(line, "Model: ") {
			cfg.Model = strings.Replace(line, "Model: ", "", -1)
		}
		if strings.Contains(line, "Linux_version: ") {
			cfg.Distro = strings.Replace(line, "Linux_version: ", "", -1)
		}
		if strings.Contains(line, "App_exe: ") {
			cfg.AppExe = strings.Replace(line, "App_exe: ", "", -1)
		}
		if strings.Contains(line, "MPI_Directory: ") {
			cfg.MPIDir = strings.Replace(line, "MPI_Directory: ", "", -1)
		}
	}

	return cfg, mpiCfg
}

// GetMetadata inspects the container's image and gathers all the available metadata
func GetMetadata(imgPath string, sysCfg *sys.Config) (Config, implem.Info, error) {
	var metadata Config
	var mpiCfg implem.Info

	err := sy.CheckIntegrity(sysCfg)
	if err != nil {
		return metadata, mpiCfg, fmt.Errorf("Singularity installation has been compromised: %s", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), sys.CmdTimeout*2*time.Minute)
	defer cancel()

	var stdout, stderr bytes.Buffer
	var cmd *exec.Cmd
	if sy.IsSudoCmd("inspect", sysCfg) {
		log.Printf("Executing %s %s inspect %s\n", sysCfg.SudoBin, sysCfg.SingularityBin, imgPath)
		cmd = exec.CommandContext(ctx, sysCfg.SudoBin, sysCfg.SingularityBin, "inspect", imgPath)
	} else {
		log.Printf("Executing %s inspect %s\n", sysCfg.SingularityBin, imgPath)
		cmd = exec.CommandContext(ctx, sysCfg.SingularityBin, "inspect", imgPath)
	}
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err = cmd.Run()
	if err != nil {
		return metadata, mpiCfg, fmt.Errorf("failed to execute command - stdout: %s; stderr: %s; err: %s", stdout.String(), stderr.String(), err)
	}

	metadata, mpiCfg = parseInspectOutput(stdout.String())
	metadata.Path = imgPath
	return metadata, mpiCfg, nil
}

func getDefaultExecArgs() []string {
	return strings.Split(defaultExecArgs, " ")
}

func getBindArguments(hostMPI *implem.Info, hostBuildenv *buildenv.Info, c *Config) []string {
	var bindArgs []string

	if c.Model == BindModel {
		if c.MPIDir == "" {
			log.Println("[WARN] the path to mount MPI in the container is undefined")
		}
		bindStr := hostBuildenv.InstallDir + ":" + c.MPIDir
		bindArgs = append(bindArgs, bindStr)
	}

	return bindArgs
}

// GetExecArgs figures out the singularity exec arguments to be used for executing a container
func GetExecArgs(myHostMPICfg *implem.Info, hostBuildEnv *buildenv.Info, syContainer *Config, sysCfg *sys.Config) []string {
	args := getDefaultExecArgs()
	if sysCfg.Nopriv {
		args = append(args, "-u")
	}

	bindArgs := getBindArguments(myHostMPICfg, hostBuildEnv, syContainer)
	if len(bindArgs) > 0 {
		args = append(args, "--bind")
		args = append(args, bindArgs...)
	}

	log.Printf("-> Exec args to use: %s\n", strings.Join(args, " "))

	return args
}