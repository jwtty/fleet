# ClusterStagedUpdateRun Controller

## Overview

The ClusterStagedUpdateRun controller manages the lifecycle of ClusterStagedUpdateRun resources, which are responsible for orchestrating staged updates of cluster resources across multiple member clusters in a controlled manner.

## Controller Responsibilities

The controller has the following key responsibilities:

1. **Initialization**: When a new ClusterStagedUpdateRun is created, the controller:
   - Validates the update configuration
   - Identifies the ClusterResourceBindings that need to be updated or deleted
   - Sets up initial conditions and status

2. **Stage Management**: For each stage in the update:
   - Tracks progress through multiple stages
   - Manages updates to ClusterResourceBindings in the current stage
   - Waits for necessary approvals before proceeding
   - Validates stage completion before moving to the next stage

3. **Approval Handling**: The controller:
   - Monitors ClusterApprovalRequests associated with the update
   - Proceeds with updates only after receiving necessary approvals
   - Reconciles when approval status changes

4. **Cleanup**: When a ClusterStagedUpdateRun is deleted:
   - Removes all associated ClusterApprovalRequests
   - Cleans up finalizers

## Reconciliation Flow

The controller follows this general reconciliation flow:

1. **Initialization Check**:
   - Verifies if the ClusterStagedUpdateRun is already initialized
   - If not initialized, performs initialization steps
   - Records initialization status

2. **Validation**:
   - Ensures the update can proceed
   - Validates current stage status
   - Checks for any blocking conditions

3. **Execution**:
   - Processes updates for the current stage
   - Creates necessary ClusterApprovalRequests
   - Updates ClusterResourceBindings
   - Monitors progress

4. **Status Updates**:
   - Records progress in status
   - Updates conditions based on execution results
   - Marks stages as complete when finished

## Conditions

The controller manages several conditions:

- `Initialized`: Indicates whether the ClusterStagedUpdateRun has been properly initialized
- `Succeeded`: Indicates whether the update has completed successfully or failed

## Error Handling

The controller handles two main categories of errors:

1. **Non-retriable Errors**:
   - Initialization failures
   - Validation failures
   - Aborted updates

2. **Retriable Errors**:
   - API server communication issues
   - Temporary conflicts
   - Resource update failures

## Related Resources

The controller interacts with:

- ClusterResourceBindings
- ClusterApprovalRequests
- Member cluster resources being updated

## Example

A typical ClusterStagedUpdateRun might look like:



This example shows a two-stage update targeting four different clusters, with the update currently in progress on the first stage.