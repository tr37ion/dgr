package main

import (
	"github.com/appc/spec/schema"
	"github.com/appc/spec/schema/types"
	"github.com/blablacar/dgr/bin-dgr/common"
	"github.com/n0rad/go-erlog/errs"
	"github.com/n0rad/go-erlog/logs"
	"io/ioutil"
	"os"
	"path/filepath"
	"strconv"
)

func (aci *Aci) Build() error {
	defer aci.giveBackUserRightsToTarget()
	aci.Clean()

	logs.WithF(aci.fields).Info("Building")
	if err := os.MkdirAll(aci.target, 0777); err != nil {
		return errs.WithEF(err, aci.fields, "Cannot create target directory")
	}

	// rkt does not automatically fetch stage1-coreos.aci if used as dependency of another stage1
	rktPath, _ := common.ExecCmdGetOutput("/bin/bash", "-c", "command -v rkt")
	common.ExecCmdGetStdoutAndStderr("rkt", "--insecure-options=image", "fetch", filepath.Dir(rktPath)+"/stage1-coreos.aci")

	ImportInternalBuilderIfNeeded(aci.manifest)

	hash, err := aci.prepareBuildAci()
	if err != nil {
		logs.WithEF(err, aci.fields).Fatal("Failed to prepare build image")
	}

	debug := "false"
	if logs.IsDebugEnabled() {
		debug = "true"
	}
	if stderr, err := common.ExecCmdGetStderr("rkt",
		"--debug="+debug,
		"--set-env="+common.ENV_LOG_LEVEL+"="+logs.GetLevel().String(),
		"--set-env="+common.ENV_ACI_PATH+"="+aci.path,
		"--set-env="+common.ENV_ACI_TARGET+"="+aci.target,
		"--net=host",
		"--insecure-options=image",
		"run",
		"--uuid-file-save="+aci.target+PATH_BUILDER_UUID,
		"--interactive",
		//		`--set-env=TEMPLATER_OVERRIDE={"dns":{"nameservers":["10.11.254.253","10.11.254.254"]}}`,
		"--stage1-name="+aci.manifest.Builder.String(),
		hash,
	); err != nil {
		logs.WithF(aci.fields.WithField("stderr", stderr)).Fatal("Builder container return with failed status")
	}

	if !Args.KeepBuilder {
		if stdout, stderr, err := common.ExecCmdGetStdoutAndStderr("rkt", "rm", "--uuid-file="+aci.target+PATH_BUILDER_UUID); err != nil {
			logs.WithEF(err, aci.fields.WithField("uuid-file", aci.target+PATH_BUILDER_UUID)).
				WithField("stdout", stdout).WithField("stderr", stderr).
				Warn("Failed to remove build container")
		}
	}
	if stdout, stderr, err := common.ExecCmdGetStdoutAndStderr("rkt", "image", "rm", hash); err != nil {
		logs.WithEF(err, aci.fields.WithField("hash", hash).WithField("stdout", stdout).WithField("stderr", stderr)).
			Warn("Failed to remove build container image")
	}

	if content, err := common.ExtractManifestContentFromAci(aci.target + PATH_IMAGE_ACI); err != nil {
		logs.WithEF(err, aci.fields).Warn("Failed to write manifest.json")
	} else if err := ioutil.WriteFile(aci.target+PATH_MANIFEST_JSON, content, 0644); err != nil {
		logs.WithEF(err, aci.fields).Warn("Failed to write manifest.json")
	} else {
		uid, err := strconv.Atoi(os.Getenv("SUDO_UID")) // TODO defer chown globally
		gid, err2 := strconv.Atoi(os.Getenv("SUDO_GID"))
		if err == nil && err2 == nil {
			os.Chown(aci.target+PATH_MANIFEST_JSON, uid, gid)
		} else {
			logs.WithEF(err, aci.fields.WithField("err2", err2)).Warn("Failed to write manifest.json")
		}
	}

	return nil
}

func (aci *Aci) prepareBuildAci() (string, error) {
	if err := os.MkdirAll(aci.target+PATH_BUILDER+common.PATH_ROOTFS, 0777); err != nil {
		return "", errs.WithEF(err, aci.fields.WithField("path", aci.target+PATH_BUILDER), "Failed to create builder path")
	}

	WriteImageManifest(aci.manifest, aci.target+PATH_BUILDER+common.PATH_MANIFEST, PREFIX_BUILDER+aci.manifest.NameAndVersion.Name())
	aci.tarAci(aci.target+PATH_BUILDER, false)

	stdout, stderr, err := common.ExecCmdGetStdoutAndStderr("rkt", "--insecure-options=image", "fetch", aci.target+PATH_BUILDER+PATH_IMAGE_ACI) // TODO may not have to fetch
	if err != nil {
		return "", errs.WithEF(err, aci.fields.WithField("stderr", stderr), "fetch of builder aci failed")
	}
	return stdout, err
}

func (aci *Aci) EnsureBuilt() error {
	if _, err := os.Stat(aci.target + PATH_IMAGE_ACI); os.IsNotExist(err) {
		if err := aci.Build(); err != nil {
			return err
		}
	}
	return nil
}

func WriteImageManifest(m *AciManifest, targetFile string, projectName string) {
	name, err := types.NewACIdentifier(projectName)
	if err != nil {
		panic(err)
	}

	version := m.NameAndVersion.Version()
	if version == "" {
		version = GenerateVersion()
	}

	labels := types.Labels{}
	labels = append(labels, types.Label{Name: "version", Value: version})
	labels = append(labels, types.Label{Name: "os", Value: "linux"})
	labels = append(labels, types.Label{Name: "arch", Value: "amd64"})

	if m.Aci.App.User == "" {
		m.Aci.App.User = "0"
	}
	if m.Aci.App.Group == "" {
		m.Aci.App.Group = "0"
	}

	im := schema.BlankImageManifest()
	im.Annotations = m.Aci.Annotations

	dgrBuilderIdentifier, _ := types.NewACIdentifier(MANIFEST_DRG_BUILDER)
	dgrVersionIdentifier, _ := types.NewACIdentifier(MANIFEST_DRG_VERSION)
	im.Annotations.Set(*dgrVersionIdentifier, DgrVersion)
	im.Annotations.Set(*dgrBuilderIdentifier, m.Builder.String())
	im.Dependencies = toAppcDependencies(m.Aci.Dependencies)
	im.Name = *name
	im.Labels = labels

	if len(m.Aci.App.Exec) == 0 {
		m.Aci.App.Exec = []string{"/dgr/bin/busybox", "sh"}
	}

	im.App = &types.App{
		Exec:             m.Aci.App.Exec,
		EventHandlers:    []types.EventHandler{{Name: "pre-start", Exec: []string{"/dgr/bin/prestart"}}},
		User:             m.Aci.App.User,
		Group:            m.Aci.App.Group,
		WorkingDirectory: m.Aci.App.WorkingDirectory,
		Environment:      m.Aci.App.Environment,
		MountPoints:      m.Aci.App.MountPoints,
		Ports:            m.Aci.App.Ports,
		Isolators:        m.Aci.App.Isolators,
	}

	buff, err := im.MarshalJSON()
	if err != nil {
		panic(err)
	}
	err = ioutil.WriteFile(targetFile, buff, 0644)
	if err != nil {
		panic(err)
	}
}

func toAppcDependencies(dependencies []common.ACFullname) types.Dependencies {
	appcDependencies := types.Dependencies{}
	for _, dep := range dependencies {
		id, err := types.NewACIdentifier(dep.Name())
		if err != nil {
			panic(err)
		}
		t := types.Dependency{ImageName: *id}
		if dep.Version() != "" {
			t.Labels = types.Labels{}
			t.Labels = append(t.Labels, types.Label{Name: "version", Value: dep.Version()})
		}

		appcDependencies = append(appcDependencies, t)
	}
	return appcDependencies
}