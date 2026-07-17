# Story 001: Initialize F5 Gardener Extension (Build Setup + CRDs)

**Type:** Story  
**Summary:** Set up build/tooling for `gardener-extension-f5` and define the initial `F5LoadBalancerConfig` CRD.

---

## Description

Implement the basic technical foundation required to start developing the F5 Gardener extension:

1. **Project/Build Setup**
   - Finalize Go module configuration for the extension.
   - Add a `Makefile` with basic developer workflows (build, test, lint, docker).
   - Add a `Dockerfile` to build a runnable controller image.
   - Add a minimal CI workflow (build + unit tests on push/PR).

2. **CRD Definition**
   - Define the `F5LoadBalancerConfig` API (Go types + validation).
   - Generate the corresponding Kubernetes CRD YAML.
   - Provide example YAMLs for different use cases (basic, advanced, production-like).

The repository structure is already present; this story focuses on **wiring it up** and defining the **first concrete API** for F5 configuration.

---

## Business Value

Provides a consistent, repeatable foundation for developing and testing the F5 extension, reduces friction for future stories, and defines the initial user-facing API (`F5LoadBalancerConfig`) that other components will rely on.

---

## Acceptance Criteria

1. **Go module and basic build work**
   - `go.mod` exists and is correctly configured (module path, Go version, core deps).
   - `go build ./...` succeeds in the repo.

2. **Makefile available**
   - `make build` builds the controller binary.
   - `make test` runs `go test ./...`.
   - `make docker-build` (or similar) builds a Docker image using the Dockerfile.
   - All these targets run successfully on the VM.

3. **Dockerfile builds runnable image**
   - `docker build` completes without errors.
   - Container starts and logs a basic startup message (even if controller logic is still a stub).

4. **Basic CI pipeline in place**
   - A workflow exists under `.github/workflows/` (or chosen CI system).
   - On push/PR to the repo:
     - Runs `go build ./...`
     - Runs `go test ./...`
   - Pipeline fails if build or tests fail.

5. **`F5LoadBalancerConfig` API defined in Go**
   - Go types added under `pkg/apis/...` for `F5LoadBalancerConfig` (spec + status).
   - Spec includes at least:
     - F5 device connection details (endpoint, partition, credentials secret ref).
     - Virtual IP and virtual server settings (port, protocol, etc.).
     - Pool configuration (LB mode, members).
     - Health monitor and basic SSL/TLS fields (even if some are placeholders for now).
   - Types compile and are registered in the scheme.

6. **CRD manifest generated**
   - A CRD YAML for `F5LoadBalancerConfig` exists under `config/` or similar.
   - `kubectl apply -f <crd-yaml>` works without schema/validation errors.

7. **Example YAMLs provided**
   - At least 2 example manifests for `F5LoadBalancerConfig`:
     - Basic example (minimal fields required to work).
     - Advanced example (uses more options like SSL / health checks).
   - Examples apply cleanly once the CRD is installed (schema validation passes).

---

## Suggested Sub‑Tasks

1. **Finalize Go module and stub main**
   - Ensure `go.mod` is correct and compiles with a minimal `cmd/gardener-extension-f5/main.go`.

2. **Create/Update Makefile**
   - Implement `build`, `test`, `docker-build` (and optionally `lint`, `clean`).

3. **Add Dockerfile**
   - Multi-stage Go build that produces a minimal runtime image.
   - Verify local image build and container startup on the VM.

4. **Add CI Workflow**
   - Create CI config to run `go build ./...` and `go test ./...` on push/PR.

5. **Define `F5LoadBalancerConfig` Go types**
   - Add spec/status structs, validation tags, and scheme registration.

6. **Generate CRD YAML**
   - Use controller-tools / kubebuilder-style generation (or manual if needed) to produce the CRD manifest.

7. **Create example CRD manifests**
   - Add basic and advanced example YAMLs under `examples/`.