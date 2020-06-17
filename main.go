package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/bitrise-io/go-steputils/stepconf"
	"github.com/bitrise-io/go-steputils/tools"
	"github.com/bitrise-io/go-utils/log"
	"google.golang.org/api/androidpublisher/v3"
	"google.golang.org/api/option"
)

func failf(format string, v ...interface{}) {
	log.Errorf(format, v...)
	os.Exit(1)
}

// uploadApplications uploads every application file (apk or aab) to the Google Play. Returns the version codes of
// the uploaded apps.
func uploadApplications(configs Configs, service *androidpublisher.Service, appEdit *androidpublisher.AppEdit) ([]int64, error) {
	var versionCodes []int64
	appPaths, _ := configs.appPaths()

	expansionFilePaths, err := expansionFiles(appPaths, configs.ExpansionfilePath)
	if err != nil {
		return []int64{}, err
	}

	for i, appPath := range appPaths {
		log.Printf("Uploading %v %d/%d", appPath, i+1, len(appPaths))
		versionCode := int64(0)
		appFile, err := os.Open(appPath)
		if err != nil {
			return []int64{}, fmt.Errorf("failed to open app (%s), error: %s", appPath, err)
		}

		if strings.ToLower(filepath.Ext(appPath)) == ".aab" {
			bundle, err := uploadAppBundle(service, configs.PackageName, appEdit.Id, appFile)
			if err != nil {
				return []int64{}, err
			}
			versionCodes = append(versionCodes, bundle.VersionCode)
			versionCode = bundle.VersionCode
		} else {
			apk, err := uploadAppApk(service, configs.PackageName, appEdit.Id, appFile)
			if err != nil {
				return []int64{}, err
			}
			versionCodes = append(versionCodes, apk.VersionCode)
			versionCode = apk.VersionCode

			if len(expansionFilePaths) > 0 {
				if err := uploadExpansionFiles(service, expansionFilePaths[i], configs.PackageName, appEdit.Id, versionCode); err != nil {
					return []int64{}, err
				}
			}
		}

		// Upload mapping.txt
		if configs.MappingFile != "" && versionCode != 0 {
			if err := uploadMappingFile(service, configs, appEdit.Id, versionCode); err != nil {
				return []int64{}, err
			}
			if i < len(appPaths)-1 {
				fmt.Println()
			}
		}
	}
	log.Printf("Done uploading of %v apps", len(appPaths))
	log.Printf("New version codes to upload: %v", versionCodes)
	return versionCodes, nil
}

// updateTracks updates the given track with a new release with the given version codes.
func updateTracks(configs Configs, service *androidpublisher.Service, appEdit *androidpublisher.AppEdit, versionCodes []int64) error {
	editsTracksService := androidpublisher.NewEditsTracksService(service)

	allTracks, err := getAllTracks(configs.PackageName, service, appEdit)
	if err != nil {
		return fmt.Errorf("failed to list tracks, error: %s", err)
	}

	trackToUpdate, err := getTrack(configs.Track, allTracks)
	if err != nil {
		return err
	}
	log.Infof("%s track will be updated.", trackToUpdate.Track)

	newRelease, err := createTrackRelease(configs.WhatsnewsDir, versionCodes, configs.UserFraction)
	if err != nil {
		return err
	}

	// Note we get error if we creating multiple instances of a release with the Completed status.
	// Example: "error: googleapi: Error 400: Too many completed releases specified., releasesTooManyCompletedReleases".
	// Also receiving error when deploying a Completed release when a rollout is in progress:
	// error: googleapi: Error 403: You cannot rollout this release because it does not allow any existing users to upgrade
	// to the newly added APKs., ReleaseValidationErrorKeyApkNoUpgradePaths

	// inProgress preserves complete release even if not specified in releases array.
	// In case only a completed release specified, it halts inProgress releases.

	trackToUpdate.Releases = []*androidpublisher.TrackRelease{newRelease}

	editsTracksUpdateCall := editsTracksService.Update(configs.PackageName, appEdit.Id, configs.Track, trackToUpdate)
	track, err := editsTracksUpdateCall.Do()
	if err != nil {
		return fmt.Errorf("update call failed, error: %s", err)
	}

	log.Printf(" updated track: %s", track.Track)
	return nil
}

func trackFlow(configs Configs, service *androidpublisher.Service) {
	//
	// Create insert edit
	fmt.Println()
	log.Infof("Create new edit")
	editsService := androidpublisher.NewEditsService(service)
	editsInsertCall := editsService.Insert(configs.PackageName, &androidpublisher.AppEdit{})
	appEdit, err := editsInsertCall.Do()
	if err != nil {
		failf("Failed to perform edit insert call, error: %s", err)
	}
	log.Printf(" editID: %s", appEdit.Id)
	log.Donef("Edit insert created")

	//
	// Upload applications
	fmt.Println()
	log.Infof("Upload apks or app bundles")
	versionCodes, err := uploadApplications(configs, service, appEdit)
	if err != nil {
		failf("Failed to upload APKs: %v", err)
	}
	log.Donef("Applications uploaded")

	// Update track
	fmt.Println()
	log.Infof("Update track")
	if err := updateTracks(configs, service, appEdit, versionCodes); err != nil {
		failf("Failed to update track, reason: %v", err)
	}
	log.Donef("Track updated")

	//
	// Validate edit
	fmt.Println()
	log.Infof("Validating edit")
	editsValidateCall := editsService.Validate(configs.PackageName, appEdit.Id)
	if _, err := editsValidateCall.Do(); err != nil {
		failf("Failed to validate edit, error: %s", err)
	}
	log.Donef("Edit is valid")

	//
	// Commit edit
	fmt.Println()
	log.Infof("Committing edit")
	editsCommitCall := editsService.Commit(configs.PackageName, appEdit.Id)
	if _, err := editsCommitCall.Do(); err != nil {
		failf("Failed to commit edit, error: %s", err)
	}

	log.Donef("Edit committed")
}

func internalAppSharingFlow(configs Configs, service *androidpublisher.Service) {
	//
	// Upload APK
	fmt.Println()
	log.Infof("Uploading internal app sharing APK")

	appPaths, _ := configs.appPaths()
	appPath := appPaths[0]
	appFile, err := os.Open(appPath)
	if err != nil {
		fmt.Errorf("failed to open app (%s), error: %s", appPath, err)
	}

	artifact, err := uploadInternalAppSharingApk(service, configs.PackageName, appFile)
	if err != nil {
		failf("Failed to upload APK %s, error: %s", appFile, err)
	} else {
		tools.ExportEnvironmentWithEnvman(internalAppSharingApkUrlKey, artifact.DownloadUrl)
		log.Donef("APK uploaded to %s", artifact.DownloadUrl)
	}
}

func main() {
	//
	// Getting configs
	fmt.Println()
	log.Infof("Getting configuration")
	var configs Configs
	if err := stepconf.Parse(&configs); err != nil {
		failf("Couldn't create config: %s\n", err)
	}
	stepconf.Print(configs)
	if err := configs.validate(); err != nil {
		failf(err.Error())
	}
	log.Donef("Configuration read successfully")

	//
	// Create client and service
	fmt.Println()
	log.Infof("Authenticating")
	client, err := createHTTPClient(string(configs.JSONKeyPath))
	if err != nil {
		failf("Failed to create HTTP client: %v", err)
	}
	service, err := androidpublisher.NewService(context.TODO(), option.WithHTTPClient(client))
	if err != nil {
		failf("Failed to create publisher service, error: %s", err)
	}
	log.Donef("Authenticated client created")

	if configs.Track == internalAppSharingName {
		internalAppSharingFlow(configs, service)
	} else {
		trackFlow(configs, service)
	}
}
