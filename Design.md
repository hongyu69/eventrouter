# Design Doc: K8s Fleet Event Telemetry & Anomaly Detection System

**Author:** [Your Name/Team]  
**Date:** 2026-01-07  
**Status:** Draft  

## 1. Executive Summary
This document outlines the architecture for a centralized telemetry system designed to ingest, analyze, and act upon Kubernetes events from a fleet of thousands of clusters. The system utilizes `eventrouter` for edge collection, a high-throughput streaming platform for ingestion, real-time stream processing for anomaly detection, and a service bus for decoupled downstream consumption.

Critically, the system is designed to support a **multi-tenant** environment, providing distinct visibility for **Platform Infrastructure** (e.g., CNI, CSI, Ingress) and **Customer Workloads**. This ensures that platform SREs and customer support teams receive relevant, routed alerts without noise contamination.

## 2. Design Rationale: Custom Pipeline vs. Native Log Analytics

While Azure Kubernetes Service (AKS) offers native integration with Azure Log Analytics (ContainerLog/KubeEvents tables), this custom event-based architecture is required to address specific limitations in the native solution for **real-time, fleet-wide anomaly detection**:

*   **Ingestion Latency**: Log Analytics is primarily a store-and-query system. Ingestion latency typically ranges from 2 to 10 minutes. This delay is unacceptable for time-critical anomaly detection (e.g., detecting a cascading upgrade failure immediately). Our streaming pipeline targets **sub-second** latency.
*   **Cost Efficiency**: Log Analytics charges by data ingestion and retention. Streaming millions of raw `KubeEvents` into Log Analytics is prohibitively expensive at fleet scale. By filtering at the edge and processing in-stream, we only persist high-value anomalies, drastically reducing storage costs.
*   **Complex Event Processing (CEP)**: Log Analytics (KQL) relies on polling for alerts. A streaming engine (like Flink/ASA) allows for stateful, windowed computations (e.g., "Alert if >3 failed mounts in 1 minute") that are difficult or resource-intensive to implement via periodic KQL queries.
*   **Open Integration**: A Kafka-based backbone decoupling allows any downstream consumer (OpsGenie, Custom Auto-Remediation, Data Lake) to "plug in" to the real-time feed without relying on proprietary Azure Monitor connectors or polling APIs.

## 3. System Architecture

The data pipeline consists of four main stages: **Collection**, **Ingestion**, **Processing**, and **Distribution**.

### 2.1 High-Level Data Flow
1.  **Edge**: `eventrouter` runs on each K8s cluster, watching the API Server for events.
2.  **Ingestion**: Events are forwarded asynchronously to a central **Kafka** (or Azure Event Hubs) cluster.
3.  **Processing**: A **Stream Analytics** job consumes the raw stream, applying windowing logic to detect anomalies (e.g., CrashLoopBackOff spikes, node instability).
4.  **Distribution**: Detected anomalies are published to a **Service Bus** topic.
5.  **Consumption**: Downstream subscribers (Alerting, Dashboards, Auto-remediation) consume messages relevant to them.

## 4. Component Detail

### 4.1 Edge Collection (Source)
*   **Component**: `eventrouter` (deployed as a Deployment/DaemonSet).
*   **Responsibility**: Watch `v1.Event` resources. Transform to JSON.
*   **Edge Filtering Strategy**:
    *   **Objective**: Eliminate noise at the source to prevent "DDOS by Logging" and reduce costs.
    *   **Mechanism**: A configurable filter engine running inside `eventrouter`.
    *   **Default Policy**: **DROP** all events where `Type == 'Normal'`.
    *   **Overrides**: Allow-list specific `Normal` events (e.g., `NodeReady`) that are required for reachability analysis.
*   **Transport**: Producer API to push directly to Ingestion endpoint.
*   **Auth**: Mutual TLS (mTLS) or SAS tokens per cluster.

### 4.2 Ingestion Layer (Stream Buffer)
*   **Technology**: Apache Kafka / Azure Event Hubs.
*   **Capacity Planning**: High throughput, persistent retention (e.g., 7 days).
*   **Partitioning Key**: `ClusterID`. This ensures all events from a specific cluster land in the same partition, preserving order for stateful analysis.

### 4.3 Stream Processing (Analysis)
*   **Technology**: Apache Flink / Azure Stream Analytics / KSQL.
*   **Logic**:
    *   **Filtering**: Ignore `Normal` events, focus on `Warning`.
    *   **Enrichment/Classification**: Tag events based on Namespace/Labels to distinguish **Platform** (e.g., `kube-system`, `gatekeeper-system`) vs. **Customer** (e.g., `tenant-*`, `user-*`) workloads.
    *   **Windowing**: Tumbling or Hopping windows (e.g., 5-minute intervals).
    *   **Pattern Matching**: Detect >10 `FailedMount` or `CrashLoopBackOff` events within a window for the same namespace/pod.
*   **Output**: JSON payload describing the anomaly (ClusterID, Severity, Timestamp, Category [Platform|Customer], RawEventRefs).

### 4.4 Distribution & Consumption
*   **Technology**: Enterprise Service Bus (e.g., RabbitMQ, Azure Service Bus).
*   **Pattern**: Publish/Subscribe (Pub/Sub) with Topic filters.
*   **Routing Strategy**:
    *   **Topic**: `telemetry.anomalies`
    *   **Subscription Filters**: Consumers filter based on the `Category` field enriched during processing.
*   **Subscribers**:
    *   **Platform SRE**: Subscribes to `Category == 'Platform'`. Triggers PagerDuty to internal teams for system component failures (e.g., CNI crash).
    *   **Customer Support**: Subscribes to `Category == 'Customer'`. Feeds into CRM/Ticketing systems to proactively notify customers of their application misconfigurations.
    *   **Data Lake Sink**: Archives ALL anomalies for comprehensive historical analysis.
    *   **Enrichment Service**: Decorates events with cluster metadata (Owner, Region) before forwarding to dashboards.

## 5. Scalability & Reliability
*   **Edge Reduction**: Filtering `Normal` events at the edge is the primary scalability lever, expected to reduce volume by ~90%.
*   **Throttling**: The Edge agent (`eventrouter`) must implement rate limiting to prevent DDOSing the ingestion layer during cluster-wide failures.
*   **Backpressure**: The Ingestion layer acts as a buffer. Stream processors must scale horizontally based on lag.
*   **Idempotency**: Downstream consumers must handle duplicate deliveries (At-least-once delivery guarantee).

## 6. Security
*   **Encryption**: TLS 1.2+ in transit. Encryption at Rest in Kafka/Service Bus.
*   **Isolation**: Ingestion layer filters invalid/unauthorized Cluster IDs.

## 7. Open Questions
*   Specific definition of "Anomaly" (needs tuning).
*   Retention policy for raw events vs. anomalies.
