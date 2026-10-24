# Product Requirements Document (PRD)
**Project Name:** Multi-Tenant Point Calculation Engine  
**Platform:** Go-based Microservice  
**Document Version:** 1.0  

## 1. Executive Summary
The Point Calculation Engine is a highly scalable, multi-tenant backend microservice written in Go. It allows third-party vendors (tenants) to design, configure, and execute custom point-based loyalty or gamification systems. The system provides a centralized configuration panel API for architects and a high-throughput execution API for processing point transactions securely, enforcing rules like cooldowns and rate limits.

## 2. Core Concepts & Terminology
* **Tenant (Vendor):** A business or platform utilizing the service to manage points for their own user base.
* **Architecture Key (Entity Key):** The primary identifier used by a specific tenant to attach points to their objects (e.g., `user_id`, `wallet_address`, `device_id`, `company_id`).
* **Tier (Package):** A grouping mechanism for rules (e.g., Basic, Gold, Platinum). Tiers define the context under which rules operate.
* **Rule (Action/Sub-package):** A specific, triggerable event that results in a point calculation (e.g., "Daily Login", "Purchase Item").
* **Entity Profile:** The current state and balance of an individual object tracked under the Architecture Key.
* **Ledger:** The append-only audit trail of every point transaction.

---

## 3. Functional Requirements

### 3.1 Tenant Onboarding & Architecture Configuration
* **System Architecture Definition:** Upon onboarding, the tenant must define their `Architecture Key` (e.g., `user_id`). All subsequent data storage and point attribution for this tenant will pivot on this key.
* **Tier Management:** 
  * Tenants can create, update, and delete Tiers (e.g., Basic, Gold, Platinum).
  * Tiers can be toggled Active/Inactive.
* **Rule Management:**
  * Tenants can define customized rules within each Tier.
  * Required fields for a Rule:
    * **Name/Slug:** A unique identifier for the endpoint generation (e.g., `daily_login`).
    * **Point Delta:** The exact value to add (positive integer) or subtract (negative integer).
    * **Cooldown Period:** The minimum time required between consecutive triggers by the same entity (e.g., 60 seconds).
    * **Throttling (Rate Limit):** Maximum allowed triggers within a specific time window (e.g., max 5 times per 24 hours).
  * Rules can be dynamically toggled On/Off without deleting them. Disabled rules must immediately reject incoming execution requests.

### 3.2 Point Execution & Calculation (The Engine)
* **Dynamic API Endpoint:** The system must expose a unified execution endpoint that accepts the Tier, Rule Slug, and Entity ID.
* **Validation & Security:**
  * Authenticate the request via Tenant API Keys.
  * Verify that the requested Tier and Rule are currently *Active*.
* **Limit Enforcement:** 
  * The system must strictly enforce the Cooldown and Throttling settings defined in the Rule. Requests violating these limits must be rejected with specific HTTP error codes (e.g., `429 Too Many Requests`).
* **Atomic Transactions:** 
  * The system must calculate the new balance (Current Balance + Point Delta) and persist the updated balance.
  * The balance update and the creation of the audit log must happen in a single, atomic database transaction to prevent data corruption.
* **Idempotency:** The execution API must support idempotency keys to safely handle network retries and prevent accidental double-crediting.

### 3.3 Reporting & Logging
* **Individual Entity Lookup:**
  * The system must allow querying an individual entity's current profile (current tier, total balance) based on the Architecture Key.
  * The system must allow querying the full, paginated chronological log of all point transactions for a specific entity.
* **Global Tenant Lookup:**
  * The tenant must be able to query all profiles across their environment.
  * The tenant must be able to pull system-wide audit logs, filterable by date, tier, and rule.

---

## 4. Non-Functional Requirements

### 4.1 Performance & Scalability
* **High Throughput:** As a core microservice, the execution endpoints must be capable of handling thousands of requests per second. (Caching mechanisms like Redis should be used for evaluating cooldowns and throttles).
* **Low Latency:** Point executions should process in under 50ms (p95).
* **Statelessness:** The Go application instances must be entirely stateless to allow horizontal auto-scaling based on CPU/Memory load.

### 4.2 Data Integrity & Concurrency
* **Race Condition Prevention:** If an entity triggers multiple points simultaneously, the system must utilize row-level locking or distributed locks to ensure the balance is calculated sequentially and accurately.
* **Append-Only Ledger:** Financial/Point integrity requires that transaction logs are never mutated or deleted. Adjustments must be made via compensating transactions.

### 4.3 Multi-Tenant Isolation
* **Data Separation:** While a single database may be used, every query, update, and read must be strictly scoped to the `tenant_id` to prevent cross-contamination of user data.

---

## 5. High-Level API Surface Area

### 5.1 Management/Admin API (For the Vendor Panel)
* `POST /admin/architecture` (Set the base key)
* `CRUD /admin/tiers` (Manage packages like Basic/Gold)
* `CRUD /admin/tiers/{tier_id}/rules` (Manage sub-structures/deltas/limits)
* `PATCH /admin/tiers/{tier_id}/rules/{rule_id}/status` (Toggle ON/OFF)

### 5.2 Execution API (For the Vendor's Application)
* `POST /engine/execute`
  * Payload: `{ "entity_id": "user123", "tier": "basic", "rule": "daily_login", "idempotency_key": "abc-123" }`

### 5.3 Query & Log API
* `GET /data/profiles/{entity_id}` (Get current balance)
* `GET /data/profiles/{entity_id}/logs` (Get individual object log)
* `GET /data/logs` (Get all logs for the tenant)

---

## 6. Out of Scope for Phase 1
* Frontend Vendor Dashboard UI (This spec covers the backend API only).
* Automatic Tier Upgrades (Logic for moving users from Basic to Gold based on lifetime points; this can be added in Phase 2).
* Real-time Webhooks/WebSockets for pushing balance updates to clients.