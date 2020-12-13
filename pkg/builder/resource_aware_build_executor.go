package builder

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"sync"
	"time"

	remoteexecution "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/buildbarn/bb-remote-execution/pkg/filesystem"
	"github.com/buildbarn/bb-remote-execution/pkg/proto/remoteworker"
	"github.com/buildbarn/bb-storage/pkg/clock"
	"github.com/buildbarn/bb-storage/pkg/digest"
	"github.com/buildbarn/bb-storage/pkg/util"
	"github.com/golang/protobuf/ptypes/empty"
)

type ResourceManager struct {
	resourceLimits map[string]int64
	resourcesAvailable map[string]int64
	lock sync.Mutex
	resourceUpdates chan struct{}
	resourceAcquisitionTimeout time.Duration
	clock clock.Clock
}

func NewResourceManager(resourceLimits map[string]int64, resourceAcquisitionTimeout time.Duration) *ResourceManager {
	resourcesAvailable := map[string]int64{}
	for k, v := range resourceLimits {
		resourcesAvailable[k] = v
	}
	return &ResourceManager{
		resourceLimits: resourceLimits,
		resourcesAvailable: resourcesAvailable,
		resourceUpdates: make(chan struct{}),
		resourceAcquisitionTimeout: resourceAcquisitionTimeout,
		clock: clock.SystemClock,
	}
}

func (rm *ResourceManager) getResourceRequirements(request *remoteworker.DesiredState_Executing) (map[string]int64, error) {
	requirements := map[string]int64{}
	for resource, _ := range rm.resourceLimits {
		requirements[resource] = 1
	}
	for _, property := range request.Command.Platform.Properties {
		if _, ok := rm.resourceLimits[property.Name]; ok {
			if count, err := strconv.ParseInt(property.Value, 10, 64); err == nil {
				requirements[property.Name] = count
			} else {
				return nil, fmt.Errorf("Could not parse resource count: %s=%s", property.Name, property.Value)
			}
		}
	}
	return requirements, nil
}

func (rm *ResourceManager) isCompatible(requirements map[string]int64) bool {
	for resource, requested := range requirements {
		limit, ok := rm.resourceLimits[resource]
		if !ok || limit < requested {
			return false
		}
	}
	return true
}

func (rm *ResourceManager) reserveBlocking(ctx context.Context, requirements map[string]int64) error {
	if !rm.isCompatible(requirements) {
		return fmt.Errorf("Could not satisfy requirements on this runner: %s requested, but only %s available", requirements, rm.resourceLimits)
	}

	timer, timerChannel := rm.clock.NewTimer(rm.resourceAcquisitionTimeout)
	for {
		if rm.reserve(requirements) {
			timer.Stop()
			return nil
		}

		select {
		case <-ctx.Done():
			timer.Stop()
			return util.StatusFromContext(ctx)
		case <-timerChannel:
			return fmt.Errorf("Timed out waiting for resources; try again later")
		case <-rm.resourceUpdates:
		}
	}
}

func (rm *ResourceManager) reserve(requirements map[string]int64) bool {
	rm.lock.Lock()
	defer rm.lock.Unlock()
	for resource, requested := range requirements {
		if rm.resourcesAvailable[resource] < requested {
			return false
		}
	}
	for resource, requested := range requirements {
		rm.resourcesAvailable[resource] -= requested
	}
	return true
}

func (rm *ResourceManager) free(requirements map[string]int64) {
	rm.lock.Lock()
	defer rm.lock.Unlock()
	for resource, requested := range requirements {
		if rm.resourcesAvailable[resource] + requested > rm.resourceLimits[resource] {
			log.Fatal("Double free or corruption")
		}
		rm.resourcesAvailable[resource] += requested
	}

	// Notify that resources have been freed.
	select {
	case rm.resourceUpdates <- struct{}{}:
	default:
	}
}

type resourceAwareBuildExecutor struct {
	base			BuildExecutor
	resourceManager *ResourceManager
}

func NewResourceAwareBuildExecutor(base BuildExecutor, resourceManager *ResourceManager) BuildExecutor {
	return &resourceAwareBuildExecutor{
		base,
		resourceManager,
	}
}

func (be *resourceAwareBuildExecutor) Execute(ctx context.Context, filePool filesystem.FilePool, instanceName digest.InstanceName, request *remoteworker.DesiredState_Executing, executionStateUpdates chan<- *remoteworker.CurrentState_Executing) *remoteexecution.ExecuteResponse {
	if be.resourceManager == nil {
		return be.base.Execute(ctx, filePool, instanceName, request, executionStateUpdates)
	}

	response := &remoteexecution.ExecuteResponse{
		Result: &remoteexecution.ActionResult{
			ExecutionMetadata: &remoteexecution.ExecutedActionMetadata{},
		},
	}

	requirements, err := be.resourceManager.getResourceRequirements(request)
	if err == nil {
		executionStateUpdates <- &remoteworker.CurrentState_Executing{
			ActionDigest: request.ActionDigest,
			ExecutionState: &remoteworker.CurrentState_Executing_AcquiringResources{
				AcquiringResources: &empty.Empty{},
			},
		}
		err = be.resourceManager.reserveBlocking(ctx, requirements)
	}
	if err != nil {
		attachErrorToExecuteResponse(response, util.StatusWrap(err, "Failed to acquire resources for action"))
		return response
	}

	defer be.resourceManager.free(requirements)
	return be.base.Execute(ctx, filePool, instanceName, request, executionStateUpdates)
}
