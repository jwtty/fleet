/*
Copyright (c) Microsoft Corporation.
Licensed under the MIT license.
*/

package updaterun

import (
	"context"
	"fmt"

	placementv1alpha1 "go.goms.io/fleet/apis/placement/v1alpha1"
	placementv1beta1 "go.goms.io/fleet/apis/placement/v1beta1"
	"go.goms.io/fleet/pkg/utils/condition"
	"go.goms.io/fleet/pkg/utils/controller"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
)

// validate validates the clusterStagedUpdateRun status and ensures the update can be continued.
// The function returns the index of the stage that is updating, and the list of clusters that are scheduled to be deleted.
// If the updating stage index is -1, it means all stages are finished, and the clusterStageUpdateRun should be marked as finished.
// If the updating stage index is 0, the next stage to be updated is the first stage.
// If the updating stage index is len(updateRun.Status.StagesStatus), the next stage to be updated will be the delete stage.
func (r *Reconciler) validate(
	ctx context.Context,
	updateRun *placementv1alpha1.ClusterStagedUpdateRun,
) (int, []*placementv1beta1.ClusterResourceBinding, []*placementv1beta1.ClusterResourceBinding, error) {
	// Some of the validating function changes the object, so we need to make a copy of the object.
	updateRunRef := klog.KObj(updateRun)
	updateRunCopy := updateRun.DeepCopy()
	klog.V(2).InfoS("Start to validate the clusterStagedUpdateRun", "clusterStagedUpdateRun", updateRunRef)

	// Validate the ClusterResourcePlacement object referenced by the ClusterStagedUpdateRun.
	placementName, err := r.validateCRP(ctx, updateRunCopy)
	if err != nil {
		return -1, nil, nil, err
	}

	// Retrieve the latest policy snapshot.
	latestPolicySnapshot, clusterCount, err := r.determinePolicySnapshot(ctx, placementName, updateRunCopy)
	if err != nil {
		return -1, nil, nil, err
	}
	// Make sure the latestPolicySnapshot has not changed.
	if updateRun.Status.PolicySnapshotIndexUsed != latestPolicySnapshot.Name {
		mismatchErr := fmt.Errorf("the policy snapshot index used in the clusterStagedUpdateRun is outdated, latest: %s, recorded: %s", latestPolicySnapshot.Name, updateRun.Status.PolicySnapshotIndexUsed)
		klog.ErrorS(mismatchErr, "there's a new latest policy snapshot", "clusterResourcePlacement", placementName, "clusterStagedUpdateRun", updateRunRef)
		return -1, nil, nil, fmt.Errorf("%w: %s", errStagedUpdatedAborted, mismatchErr.Error())
	}
	// Make sure the cluster count in the policy snapshot has not changed.
	if updateRun.Status.PolicyObservedClusterCount != clusterCount {
		mismatchErr := fmt.Errorf("the cluster count initialized in the clusterStagedUpdateRun is outdated, latest: %d, recorded: %d", clusterCount, updateRun.Status.PolicyObservedClusterCount)
		klog.ErrorS(mismatchErr, "the cluster count in the policy snapshot has changed", "clusterResourcePlacement", placementName, "clusterStagedUpdateRun", updateRunRef)
		return -1, nil, nil, fmt.Errorf("%w: %s", errStagedUpdatedAborted, mismatchErr.Error())
	}

	// Collect the clusters by the corresponding ClusterResourcePlacement with the latest policy snapshot.
	scheduledBindings, toBeDeletedBindings, err := r.collectScheduledClusters(ctx, placementName, latestPolicySnapshot, updateRunCopy)
	if err != nil {
		return -1, nil, nil, err
	}

	// Validate the applyStrategy
	if updateRun.Status.ApplyStrategy == nil {
		unexpectedErr := fmt.Errorf("the clusterStagedUpdateRun has no applyStrategy")
		klog.ErrorS(controller.NewUnexpectedBehaviorError(unexpectedErr), "Failed to find the applyStrategy in the clusterStagedUpdateRun status", "clusterStagedUpdateRun", updateRunRef)
		return -1, nil, nil, fmt.Errorf("%w: %s", errStagedUpdatedAborted, unexpectedErr.Error())
	}
	if updateRun.Status.StagedUpdateStrategySnapshot == nil {
		unexpectedErr := fmt.Errorf("the clusterStagedUpdateRun has no stagedUpdateStrategySnapshot")
		klog.ErrorS(controller.NewUnexpectedBehaviorError(unexpectedErr), "Failed to find the stagedUpdateStrategySnapshot in the clusterStagedUpdateRun", "clusterStagedUpdateRun", updateRunRef)
		return -1, nil, nil, fmt.Errorf("%w: %s", errStagedUpdatedAborted, unexpectedErr.Error())
	}

	if condition.IsConditionStatusFalse(meta.FindStatusCondition(updateRun.Status.Conditions, string(placementv1alpha1.StagedUpdateRunConditionProgressing)), updateRun.Generation) {
		// The clusterStagedUpdateRun has not started yet.
		klog.V(2).InfoS("Starting the staged update run from the beginning", "clusterStagedUpdateRun", updateRunRef)
		return 0, scheduledBindings, toBeDeletedBindings, nil
	}

	// updateRun already started, find the next stage to update.
	updatingStageIndex, err := r.validateStagesStatus(ctx, scheduledBindings, updateRun)
	if err != nil {
		return -1, nil, nil, err
	}
	return updatingStageIndex, scheduledBindings, toBeDeletedBindings, nil
}

// validateStagesStatus validates both the update and delete stages of the ClusterStagedUpdateRun.
// The function returns the stage index that is updating, or any error encountered.
// If the updating stage index is -1, it means all stages are finished, and the clusterStageUpdateRun should be marked as finished.
// If the updating stage index is 0, the next stage to be updated will be the first stage.
// If the updating stage index is len(updateRun.Status.StagesStatus), the next stage to be updated will be the delete stage.
func (r *Reconciler) validateStagesStatus(ctx context.Context, scheduledBindings []*placementv1beta1.ClusterResourceBinding, updateRun *placementv1alpha1.ClusterStagedUpdateRun) (int, error) {
	// Take a copy of existing updateRun status.
	existingStageStatus := updateRun.Status.StagesStatus
	existingDeleteStageStatus := updateRun.Status.DeletionStageStatus
	updateRunCopy := updateRun.DeepCopy()
	updateRunRef := klog.KObj(updateRun)
	// Compute the stage status which does not include the delete stage.
	if err := r.computeRunStageStatus(ctx, scheduledBindings, updateRunCopy); err != nil {
		return -1, err
	}
	// validate the stages in the updateRun and return the updating stage index.
	updatingStageIndex, lastFinishedStageIndex, validateErr := validateUpdateStageStatus(existingStageStatus, updateRunCopy)
	if validateErr != nil {
		return -1, validateErr
	}

	deleteStageFinishedCond := meta.FindStatusCondition(existingDeleteStageStatus.Conditions, string(placementv1alpha1.StagedUpdateRunConditionSucceeded))
	deleteStageProgressingCond := meta.FindStatusCondition(existingDeleteStageStatus.Conditions, string(placementv1alpha1.StagedUpdateRunConditionProgressing))
	// Check if there is any active updating stage
	if updatingStageIndex != -1 || lastFinishedStageIndex < len(existingStageStatus)-1 {
		// There are still stages updating before the delete stage, make sure the delete stage is not active/finished.
		if condition.IsConditionStatusTrue(deleteStageFinishedCond, updateRun.Generation) ||
			condition.IsConditionStatusFalse(deleteStageFinishedCond, updateRun.Generation) ||
			condition.IsConditionStatusTrue(deleteStageProgressingCond, updateRun.Generation) {
			unexpectedErr := controller.NewUnexpectedBehaviorError(fmt.Errorf("the delete stage is active, but there are still stages updating, updatingStageIndex: %d, lastFinishedStageIndex: %d", updatingStageIndex, lastFinishedStageIndex))
			klog.ErrorS(unexpectedErr, "the delete stage is active, but there are still stages updating", "clusterStagedUpdateRun", updateRunRef)
			return -1, fmt.Errorf("%w: %s", errStagedUpdatedAborted, unexpectedErr.Error())
		}

		// If no stage is updating, continue from the last finished stage (which will result it starting from 0).
		if updatingStageIndex == -1 {
			updatingStageIndex = lastFinishedStageIndex + 1
		}
		return updatingStageIndex, nil
	}

	klog.InfoS("All stages are finished, continue from the delete stage", "clusterStagedUpdateRun", updateRunRef)
	// Check if the delete stage has finished successfully.
	if condition.IsConditionStatusTrue(deleteStageFinishedCond, updateRun.Generation) {
		klog.InfoS("The delete stage has finished successfully, no more stages to update", "clusterStagedUpdateRun", updateRunRef)
		return -1, nil
	}
	// Check if the delete stage has failed.
	if condition.IsConditionStatusFalse(deleteStageFinishedCond, updateRun.Generation) {
		failedErr := fmt.Errorf("the delete stage has failed, err: %s", deleteStageFinishedCond.Message)
		klog.ErrorS(failedErr, "The delete stage has failed", "stageCond", deleteStageFinishedCond, "clusterStagedUpdateRun", updateRunRef)
		return -1, fmt.Errorf("%w: %s", errStagedUpdatedAborted, failedErr.Error())
	}
	// The delete stage is still updating.
	if condition.IsConditionStatusTrue(deleteStageProgressingCond, updateRun.Generation) {
		klog.InfoS("The delete stage is updating", "clusterStagedUpdateRun", updateRunRef)
		return len(existingStageStatus), nil
	}
	// All stages have finished, but the delete stage is not active or finished.
	unexpectedErr := controller.NewUnexpectedBehaviorError(fmt.Errorf("the delete stage is not active, but all stages finished, updatingStageIndex: %d, lastFinishedStageIndex: %d", updatingStageIndex, lastFinishedStageIndex))
	klog.ErrorS(unexpectedErr, "There is no stage active", "clusterStagedUpdateRun", updateRunRef)
	return -1, fmt.Errorf("%w: %s", errStagedUpdatedAborted, unexpectedErr.Error())
}

// validateUpdateStageStatus is a helper function to validate the updating stages in the clusterStagedUpdateRun.
// It compares the existing stage status with the latest list of clusters to be updated.
// It returns the index of the updating stage, the index of the last finished stage and any error encountered.
func validateUpdateStageStatus(existingStageStatus []placementv1alpha1.StageUpdatingStatus, updateRun *placementv1alpha1.ClusterStagedUpdateRun) (int, int, error) {
	updatingStageIndex := -1
	lastFinishedStageIndex := -1
	// Remember the newly computed stage status.
	newStageStatus := updateRun.Status.StagesStatus
	// Make sure the number of stages in the clusterStagedUpdateRun are still the same.
	if len(existingStageStatus) != len(newStageStatus) {
		mismatchErr := fmt.Errorf("the number of stages in the clusterStagedUpdateRun has changed, new: %d, existing: %d", len(newStageStatus), len(existingStageStatus))
		klog.ErrorS(mismatchErr, "The number of stages in the clusterStagedUpdateRun has changed", "clusterStagedUpdateRun", klog.KObj(updateRun))
		return -1, -1, fmt.Errorf("%w: %s", errStagedUpdatedAborted, mismatchErr.Error())
	}
	// Make sure the stages in the updateRun are still the same.
	for curStage := range existingStageStatus {
		if existingStageStatus[curStage].StageName != newStageStatus[curStage].StageName {
			mismatchErr := fmt.Errorf("the `%d` stage name in the clusterStagedUpdateRun has changed, new: %s, existing: %s", curStage, newStageStatus[curStage].StageName, existingStageStatus[curStage].StageName)
			klog.ErrorS(mismatchErr, "The stage name in the clusterStagedUpdateRun has changed", "clusterStagedUpdateRun", klog.KObj(updateRun))
			return -1, -1, fmt.Errorf("%w: %s", errStagedUpdatedAborted, mismatchErr.Error())
		}
		if len(existingStageStatus[curStage].Clusters) != len(newStageStatus[curStage].Clusters) {
			mismatchErr := fmt.Errorf("the number of clusters in the `%d` stage has changed, new: %d, existing: %d", curStage, len(newStageStatus[curStage].Clusters), len(existingStageStatus[curStage].Clusters))
			klog.ErrorS(mismatchErr, "The number of clusters in the stage has changed", "clusterStagedUpdateRun", klog.KObj(updateRun))
			return -1, -1, fmt.Errorf("%w: %s", errStagedUpdatedAborted, mismatchErr.Error())
		}
		// Check that the clusters in the stage are still the same.
		for j := range existingStageStatus[curStage].Clusters {
			if existingStageStatus[curStage].Clusters[j].ClusterName != newStageStatus[curStage].Clusters[j].ClusterName {
				mismatchErr := fmt.Errorf("the `%d` cluster in the `%d` stage has changed, new: %s, existing: %s", j, curStage, newStageStatus[curStage].Clusters[j].ClusterName, existingStageStatus[curStage].Clusters[j].ClusterName)
				klog.ErrorS(mismatchErr, "The cluster in the stage has changed", "clusterStagedUpdateRun", klog.KObj(updateRun))
				return -1, -1, fmt.Errorf("%w: %s", errStagedUpdatedAborted, mismatchErr.Error())
			}
		}

		stageSucceedCond := meta.FindStatusCondition(existingStageStatus[curStage].Conditions, string(placementv1alpha1.StageUpdatingConditionSucceeded))
		stageStartedCond := meta.FindStatusCondition(existingStageStatus[curStage].Conditions, string(placementv1alpha1.StageUpdatingConditionProgressing))
		if condition.IsConditionStatusTrue(stageSucceedCond, updateRun.Generation) {
			// The stage has finished.
			if updatingStageIndex != -1 && curStage > updatingStageIndex {
				// The finished stage is after the updating stage.
				unexpectedErr := controller.NewUnexpectedBehaviorError(fmt.Errorf("the finished stage `%d` is after the updating stage `%d`", curStage, updatingStageIndex))
				klog.ErrorS(unexpectedErr, "The finished stage is after the updating stage", "clusterStagedUpdateRun", klog.KObj(updateRun))
				return -1, -1, fmt.Errorf("%w: %s", errStagedUpdatedAborted, unexpectedErr.Error())
			}
			// Record the last finished stage so we can continue from the next stage if no stage is updating.
			lastFinishedStageIndex = curStage
			// Make sure that all the clusters are updated.
			for curCluster := range existingStageStatus[curStage].Clusters {
				// Check if the cluster is still updating.
				if condition.IsConditionStatusFalse(meta.FindStatusCondition(
					existingStageStatus[curStage].Clusters[curCluster].Conditions,
					string(placementv1alpha1.ClusterUpdatingConditionSucceeded)),
					updateRun.Generation) {
					// The clusters in the finished stage should all have finished too.
					unexpectedErr := controller.NewUnexpectedBehaviorError(fmt.Errorf("cluster `%s` in the finished stage `%s` is still updating", existingStageStatus[curStage].Clusters[curCluster].ClusterName, existingStageStatus[curStage].StageName))
					klog.ErrorS(unexpectedErr, "The cluster in a finished stage is still updating", "clusterStagedUpdateRun", klog.KObj(updateRun))
					return -1, -1, fmt.Errorf("%w: %s", errStagedUpdatedAborted, unexpectedErr.Error())
				}
			}
		} else if condition.IsConditionStatusFalse(stageSucceedCond, updateRun.Generation) {
			// The stage has failed.
			failedErr := fmt.Errorf("the stage `%s` has failed, err: %s", existingStageStatus[curStage].StageName, stageSucceedCond.Message)
			klog.ErrorS(failedErr, "The stage has failed", "stageCond", stageSucceedCond, "clusterStagedUpdateRun", klog.KObj(updateRun))
			return -1, -1, fmt.Errorf("%w: %s", errStagedUpdatedAborted, failedErr.Error())
		} else if stageStartedCond != nil {
			// The stage is still updating.
			if updatingStageIndex != -1 {
				// There should be only one stage updating at a time.
				unexpectedErr := controller.NewUnexpectedBehaviorError(fmt.Errorf("the stage `%s` is updating, but there is already a stage `%s` updating", existingStageStatus[curStage].StageName, existingStageStatus[updatingStageIndex].StageName))
				klog.ErrorS(unexpectedErr, "Detected more than one updating stages", "clusterStagedUpdateRun", klog.KObj(updateRun))
				return -1, -1, fmt.Errorf("%w: %s", errStagedUpdatedAborted, unexpectedErr.Error())
			}
			if curStage != lastFinishedStageIndex+1 {
				// The current updating stage is not right after the last finished stage.
				unexpectedErr := controller.NewUnexpectedBehaviorError(fmt.Errorf("the updating stage `%s` is not right after the last finished stage `%s`", existingStageStatus[curStage].StageName, existingStageStatus[lastFinishedStageIndex].StageName))
				klog.ErrorS(unexpectedErr, "There's not yet started stage before the updating stage", "clusterStagedUpdateRun", klog.KObj(updateRun))
				return -1, -1, fmt.Errorf("%w: %s", errStagedUpdatedAborted, unexpectedErr.Error())
			}
			updatingStageIndex = curStage
			// Collect the updating clusters
			var updatingClusters []string
			for j := range existingStageStatus[curStage].Clusters {
				clusterStartedCond := meta.FindStatusCondition(existingStageStatus[curStage].Clusters[j].Conditions, string(placementv1alpha1.ClusterUpdatingConditionStarted))
				clusterFinishedCond := meta.FindStatusCondition(existingStageStatus[curStage].Clusters[j].Conditions, string(placementv1alpha1.ClusterUpdatingConditionSucceeded))
				if condition.IsConditionStatusTrue(clusterStartedCond, updateRun.Generation) &&
					!(condition.IsConditionStatusTrue(clusterFinishedCond, updateRun.Generation) || condition.IsConditionStatusFalse(clusterFinishedCond, updateRun.Generation)) {
					updatingClusters = append(updatingClusters, existingStageStatus[curStage].Clusters[j].ClusterName)
				}
			}
			// We don't alllow more than one cluster to be updating at the same time.
			if len(updatingClusters) > 1 {
				unexpectedErr := controller.NewUnexpectedBehaviorError(fmt.Errorf("more than one cluster is updating in the stage `%s`, clusters: %v", existingStageStatus[curStage].StageName, updatingClusters))
				klog.ErrorS(unexpectedErr, "Detected more than one updating clusters in the stage", "clusterStagedUpdateRun", klog.KObj(updateRun))
				return -1, -1, fmt.Errorf("%w: %s", errStagedUpdatedAborted, unexpectedErr.Error())
			}
		}
	}
	return updatingStageIndex, lastFinishedStageIndex, nil
}

// recordUpdateRunFailed records the failed condition in the ClusterStagedUpdateRun status.
func (r *Reconciler) recordUpdateRunFailed(ctx context.Context, updateRun *placementv1alpha1.ClusterStagedUpdateRun, message string) error {
	meta.SetStatusCondition(&updateRun.Status.Conditions, metav1.Condition{
		Type:               string(placementv1alpha1.StagedUpdateRunConditionSucceeded),
		Status:             metav1.ConditionFalse,
		ObservedGeneration: updateRun.Generation,
		Reason:             condition.UpdateRunFailedReason,
		Message:            message,
	})
	if updateErr := r.Client.Status().Update(ctx, updateRun); updateErr != nil {
		klog.ErrorS(updateErr, "Failed to update the ClusterStagedUpdateRun status as failed", "clusterStagedUpdateRun", klog.KObj(updateRun))
		// updateErr can be retried.
		return controller.NewAPIServerError(false, updateErr)
	}
	return nil
}
