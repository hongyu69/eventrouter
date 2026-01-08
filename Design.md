# RFC: Cosmic Event-Based Fleet Observability System

## 1. Executive Summary
This document outlines the architecture for a centralized, multi-signal telemetry system designed to maintain a holistic view of a massive fleet of resources (including Kubernetes clusters).

The system evolves beyond simple k8s event logging into a **state-driven observability platform**. It ingests signals from various sources (Kubernetes events being a primary one) into a unified broker, processes them via azure stream analytics to update a real-time state store (Cosmos DB), and drives downstream automated repair and alerting services based on resource state transitions.

## 2. Design Rationale

### 2.1 Event Hub as Universal Buffer
We utilize an event broker (e.g., Azure Event Hubs) not just as a pipe, but as a critical **load-leveling mechanism**. This allows us to ingest high-volume, bursty traffic from thousands of clusters and other sources without overwhelming downstream databases or compute engines. It serves as the unified entry point for all fleet signals.

### 2.2 Holistic State View (Cosmos DB)
Instead of reacting to individual transient events, we aim to maintain a **persistent state** for every resource. By using an "Upsert" pattern effectively, we transform a stream of events into a live table of "Current Resource Health". This allows us to query *state* (e.g., "Give me all clusters in 'Degraded' state") rather than parsing logs.

### 2.3 Decoupled Remediation
The analysis logic is separated from the action logic.
*   **Analysis (ASA)**: Responsible for normalizing data and determining *what is happening* (updating state).
*   **Action (Repair Service)**: Responsible for determining *what to do about it* (executing repairs based on state transitions).

## 3. System Architecture

The pipeline follows a **Source -> Buffer -> State -> Action** flow.

### 3.1 High-Level Data Flow
1.  **Sources**: Resources (Subscriptions, K8s Clusters, Namespace Instances, etc.) emit events. `eventrouter` captures K8s events.
2.  **Ingestion & Buffer**: Events are pushed to **Event Hubs** partitions.
3.  **Pre-Processing (ETL)**: **Azure Stream Analytics (ASA)** consumes the raw stream, filters noise, transforms formats, and prepares data for storage.
4.  **State Persistence**: ASA outputs to **Cosmos DB** using an **Upsert** policy. This creates/updates the "Holistic View" of each resource.
5.  **Downstream Action**:
    *   **Repair Service**: Polls/Change-Feeds from Cosmos DB to detect unhealthy resources and execute mitigation logic via a State Machine.
    *   **Alerting Service**: Monitors for resources that fail repair or enter critical states and pages humans.

## 4. Component Detail

### 4.1 Sources & Edge Collection
*   **Kubernetes Clusters**: Uses `eventrouter` to watch `v1.Event`.
*   **Other Sources**: Pipeline is extensible to accept signals from VMs, network devices, or external monitoring agents.
*   **Protocol**: All sources send standardized JSON envelopes to the Ingestion Layer.

### 4.2 Ingestion Layer (Load Leveling)
*   **Component**: Azure Event Hubs / Kafka.
*   **Role**: 
    1.  **Buffer**: Absorb spikes from "Event Storms" (e.g., region-wide outages).
    2.  **Decoupling**: Sources don't need to know about the database schema or processing logic.
*   **Partitioning**: Keyed by `ResourceID` to ensure ordering for specific resources.

### 4.3 Stream Processing (ASA)
*   **Component**: Azure Stream Analytics.
*   **Logic**:
    *   **Extraction**: Pull relevant fields (ClusterID, Component, Reason, Message) from user-defined payload.
    *   **Pre-Processing**: Calculate derived fields (e.g., `IsCritical = true` if `Reason == 'CrashLoopBackOff'`).
    *   **Output**: Writes to Cosmos DB.
*   **Sinking Policy**: **Upsert**.
    *   Key: `ResourceID` (e.g., `/subscriptions/s1/resourceGroups/rg1/providers/Microsoft.ContainerService/managedClusters/c1`).
    *   Behavior: If the record exists, update the status and timestamp; if not, insert it.

### 4.4 Global State Store (The "Holistic View")
*   **Component**: Azure Cosmos DB (NoSQL API).
*   **Data Model**: document per resource.
    ```json
    {
      "id": "<ResourceID>",
      "type": "Microsoft.ContainerService/managedClusters",
      "healthState": "Unhealthy",
      "lastEventTimestamp": "2026-01-08T10:00:00Z",
      "activeSignals": [
        { "code": "K8sEvent", "reason": "EtcdDown", "count": 5 }
      ],
      "mitigationStatus": {
        "attemptCount": 0,
        "lastAction": null
      }
    }
    ```

### 4.5 Repair Service & State Machine
*   **Role**: The "Brain" of the operation.
*   **Mechanism**: Monitors Cosmos DB (via Change Feed or Polling).
*   **State Machine Transitions**:
    *   `Healthy`: No active adverse signals.
    *   `Unhealthy`: Bad signals detected. Trigger **Mitigation A** (e.g., Restart Node).
    *   `MitigationInProgress`: Wait for signals to clear.
    *   `Probation`: Mitigation failed or issue recurred. Trigger **Mitigation B** (e.g., Redeploy Control Plane).
    *   `Prohibited/Terminal`: Manual intervention required.
*   **Logic**:
    *   If `HealthState` moves from `Healthy` -> `Unhealthy`, schedule Repair Job.
    *   If repair succeeds, state returns to `Healthy`.
    *   If repair fails max retries, transition to `Terminal` and emit Alert.

### 4.6 Alerting & Reporting
*   **Consumers**:
    *   **PagerDuty**: Triggered only when the State Machine reaches `Terminal` or critical `Unhealthy` states that cannot be auto-repaired.
    *   **Dashboards**: Visualize the count of clusters in each state (e.g., "98% Healthy, 1% Mitigating, 1% Terminal").

## 5. Security & Reliability
*   **Identity**: Managed Identities for ASA -> Event Hub and ASA -> Cosmos DB.
*   **Resiliency**: 
    *   Event Hub captures data even if ASA is down.
    *   Cosmos DB provides HA and Geo-Replication for the state view.
