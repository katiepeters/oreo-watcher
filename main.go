package main

import (
	"context"

	"go.viam.com/rdk/components/audioin"
	"go.viam.com/rdk/components/camera"
	"go.viam.com/rdk/components/sensor"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/module"
	utils "go.viam.com/utils"

	activitymonitor "github.com/katie-viam/oreo-watcher/activitymonitor"
	barkmonitor "github.com/katie-viam/oreo-watcher/barkmonitor"
	filteredmic "github.com/katie-viam/oreo-watcher/filteredmic"
	learningmode "github.com/katie-viam/oreo-watcher/learningmode"
	modecontroller "github.com/katie-viam/oreo-watcher/modecontroller"
	movementmonitor "github.com/katie-viam/oreo-watcher/movementmonitor"
	sleepmonitor "github.com/katie-viam/oreo-watcher/sleepmonitor"
	spectrogramcam "github.com/katie-viam/oreo-watcher/spectrogramcam"
)

func main() {
	utils.ContextualMain(mainWithArgs, module.NewLoggerFromArgs("oreo-watcher"))
}

func mainWithArgs(ctx context.Context, args []string, logger logging.Logger) error {
	// Must happen before any resource construction, and independent of any
	// resource's dependency graph: the tflite_cpu mlmodel service backing
	// learning-mode's yamnet_service fails to construct if this file doesn't
	// exist yet, and learning-mode itself depends on that service being
	// healthy — so writing the file only from learning-mode's own
	// constructor would deadlock. See EnsureModelFile's doc comment.
	if err := learningmode.EnsureModelFile(logger); err != nil {
		logger.Errorw("failed to ensure yamnet model file at startup", "error", err)
	}

	myMod, err := module.NewModuleFromArgs(ctx)
	if err != nil {
		return err
	}

	if err = myMod.AddModelFromRegistry(ctx, audioin.API, filteredmic.Model); err != nil {
		return err
	}

	if err = myMod.AddModelFromRegistry(ctx, camera.API, spectrogramcam.Model); err != nil {
		return err
	}

	if err = myMod.AddModelFromRegistry(ctx, sensor.API, barkmonitor.Model); err != nil {
		return err
	}

	if err = myMod.AddModelFromRegistry(ctx, sensor.API, learningmode.Model); err != nil {
		return err
	}

	if err = myMod.AddModelFromRegistry(ctx, sensor.API, modecontroller.Model); err != nil {
		return err
	}

	if err = myMod.AddModelFromRegistry(ctx, sensor.API, activitymonitor.Model); err != nil {
		return err
	}

	if err = myMod.AddModelFromRegistry(ctx, sensor.API, movementmonitor.Model); err != nil {
		return err
	}

	if err = myMod.AddModelFromRegistry(ctx, sensor.API, sleepmonitor.Model); err != nil {
		return err
	}

	err = myMod.Start(ctx)
	defer myMod.Close(ctx)
	if err != nil {
		return err
	}

	<-ctx.Done()
	return nil
}
