---
name: testing-guidelines
description: General testing, coverage, isolation, contract-testing, integration-testing, and E2E quality requirements for software projects.
---

# skills/testing-guidelines

Apply these rules whenever creating, modifying, reviewing, deleting, or evaluating production code or tests.

This skill defines general testing policy.

Repository-specific test commands, tools, paths, exclusions, coverage collectors, and known baseline debt SHOULD be
defined separately by the project.

## When to use

Use this skill when:

- adding or modifying production code;
- adding, modifying, reviewing, or deleting tests;
- evaluating test quality or coverage;
- fixing test failures;
- reviewing changes that affect observable behavior;
- adding or modifying APIs, schemas, protocols, or contracts;
- changing persistence or database behavior;
- adding integration or E2E tests;
- changing security-sensitive or reliability-sensitive code.

## Instructions

### 1. Determine required testing

For every production-code change:

1. Identify the affected behavior.
2. Identify meaningful branches and state transitions.
3. Identify relevant boundaries and input classes.
4. Identify possible failure and recovery paths.
5. Identify affected public or internal contracts.
6. Determine whether existing tests already verify them.
7. Add or update tests where verification is insufficient.
8. Run the smallest relevant tests first.
9. Run the appropriate broader test suite afterward.
10. Collect coverage when reliable coverage instrumentation is available.

Tests SHOULD be selected according to risk and observable behavior rather than implementation structure alone.

Do not add tests solely to increase a coverage percentage.

Do not remove tests solely because they do not increase line coverage.

### 2. Coverage requirements

Coverage is a minimum quality gate.

Coverage is NOT proof that the implementation is correct or that the test suite is sufficient.

Unless a repository defines stricter requirements:

- overall production-code coverage MUST exceed 85%;
- added or modified production code MUST exceed 80%;
- core production code MUST exceed 90%;
- new changes MUST NOT reduce an established coverage baseline.

Existing code below these thresholds MAY remain as explicitly tracked baseline debt.

New or modified code MUST NOT use historical coverage debt as justification for introducing additional untested
behavior.

Coverage SHOULD be measured per language or independently instrumented subsystem when aggregating results would produce
misleading percentages.

When supported, both line coverage and branch coverage SHOULD be inspected.

High line coverage MUST NOT be considered sufficient when meaningful branches, error paths, state transitions, or
boundary conditions remain untested.

A missing, corrupt, or incomplete coverage report MUST be treated as a coverage collection failure.

It MUST NOT be interpreted as either:

- 0% coverage;
- successful coverage;
- an implicitly skipped coverage check.

### 3. Identifying core code

Core code is production code whose incorrect behavior would have disproportionately high impact on correctness,
compatibility, security, data integrity, or system availability.

Classification MUST be based primarily on responsibility and failure impact, not on filename, directory, code size, or
implementation language.

Code SHOULD be classified as core when one or more of the following applies:

- it implements a public API or externally relied-upon contract;
- it defines or enforces a protocol, schema, serialization format, or compatibility boundary;
- it performs authentication, authorization, permission checks, cryptographic operations, or other security-sensitive
  behavior;
- it controls persistence, migrations, transactions, consistency, or durable state;
- an error could corrupt, destroy, duplicate, leak, or irreversibly modify user data;
- it contains central business rules or domain invariants;
- many other components depend on its behavior;
- it acts as a shared foundational library or subsystem;
- it performs scheduling, coordination, concurrency control, locking, leader election, or distributed-state management;
- it controls retries, recovery, failover, rollback, or other reliability-critical behavior;
- failure could cause a system-wide outage or prevent the primary function of the product;
- backward compatibility is important and regressions would affect existing consumers;
- the code has historically produced high-severity or difficult-to-detect defects.

Code MAY also be treated as core when a project explicitly designates it as such based on its architecture or risk
model.

Examples commonly considered core include:

- parsers and protocol implementations;
- authentication and authorization logic;
- billing or financial calculations;
- storage engines and persistence layers;
- migration logic;
- shared data models with strong invariants;
- schedulers and orchestration engines;
- concurrency primitives;
- public SDK behavior;
- compatibility layers;
- critical configuration validation.

Code is NOT automatically core merely because:

- it is frequently executed;
- it is located in a `core/`, `internal/`, or similarly named directory;
- it has many lines of code;
- it is difficult to test;
- it has low existing coverage.

When classification is ambiguous, evaluate the consequence of an undetected defect.

A practical rule is:

> If a defect in the code could silently violate an important contract, compromise security or data integrity, break
> many dependents, or disable a primary system capability, treat it as core.

Core-code classification SHOULD be stable and documented when it affects CI coverage thresholds.

It SHOULD NOT be changed opportunistically merely to bypass a coverage requirement.

### 4. Test value and redundancy

A test SHOULD provide independent fault-detection value.

A test MAY provide value even when it does not increase line or branch coverage.

A test is potentially redundant only when it adds none of:

- code coverage;
- branch coverage;
- behavioral coverage;
- contract coverage;
- input equivalence-class coverage;
- boundary coverage;
- state-transition coverage;
- error or failure coverage;
- concurrency coverage;
- compatibility coverage;
- stronger or more precise assertions.

Coverage delta alone MUST NOT be used as proof that a test is redundant.

Two tests that execute identical lines MAY still verify materially different behavior.

For example:

- successful input versus invalid input;
- minimum boundary versus maximum boundary;
- authorized versus unauthorized operation;
- initial state versus transitioned state;
- successful persistence versus rollback;
- compatible input versus intentionally rejected input.

### 5. Unit tests

Unit tests MUST be:

- deterministic;
- repeatable;
- independently executable;
- self-checking;
- isolated from uncontrolled persistent external state.

Unit tests SHOULD be:

- fast;
- hermetic;
- independent of network access;
- independent of external services;
- independent of wall-clock time;
- independent of uncontrolled randomness.

Unit tests MUST NOT:

- depend on execution order;
- depend on another test having executed first;
- require manually prepared external state;
- modify shared persistent state without cleanup.

Tests SHOULD follow FIRST:

- Fast;
- Independent / Isolated;
- Repeatable;
- Self-validating;
- Timely.

Prefer assertions against observable behavior and contracts over incidental implementation details.

Do not weaken assertions merely to make a failing test pass.

### 6. Test design

For affected behavior, consider at least:

- normal behavior;
- meaningful branches;
- input equivalence classes;
- lower and upper boundaries;
- empty or missing values;
- zero values;
- null or nil values where applicable;
- malformed inputs;
- invalid states;
- error paths;
- failure recovery;
- state transitions;
- retry behavior;
- concurrency behavior where applicable;
- public API behavior;
- compatibility requirements;
- protocol and schema contracts.

Not every category applies to every change.

Test selection SHOULD be proportional to risk.

Higher-risk code SHOULD receive stronger and more diverse verification.

### 7. Persistent state and databases

Tests MUST NOT operate against production databases or production persistent state.

When a real persistent service is required, tests SHOULD use an isolated disposable instance.

The test environment SHOULD:

- be provisioned automatically;
- use test-specific credentials;
- initialize required schemas and data automatically;
- avoid shared mutable state where practical;
- clean up resources after execution.

Containerized temporary services are RECOMMENDED when they provide realistic behavior with acceptable execution cost.

Tests MUST NOT depend on manually prepared persistent data.

### 8. Integration tests

Use integration tests when correctness depends on interaction between components that cannot be meaningfully validated
in isolation.

Integration tests MAY use controlled dependencies such as:

- temporary databases;
- temporary filesystem state;
- local servers;
- containers;
- message brokers;
- caches;
- explicitly provisioned test services.

Integration tests MUST be automatically reproducible.

They SHOULD remain deterministic.

Dependencies SHOULD be provisioned and cleaned up automatically whenever practical.

Integration tests SHOULD verify component boundaries rather than duplicate every unit-test case through a larger stack.

### 9. Contract tests

Use contract tests for interfaces relied upon by independently developed components or external consumers.

Contracts MAY include:

- data schemas;
- API inputs and outputs;
- protocol messages;
- serialization formats;
- behavioral semantics;
- compatibility guarantees;
- error behavior.

Contract definitions SHOULD be implementation-independent when multiple languages or components consume them.

Transport-independent domain schemas SHOULD be separated from transport-specific concerns when practical.

For example:

- use schema definitions for shared data structures;
- use protocol-specific definitions for transport semantics;
- reference shared schemas rather than duplicating them.

Each implementation SHOULD execute conformance tests against the authoritative contract.

Contract tests SHOULD verify both:

- valid cases that implementations must accept;
- invalid or incompatible cases that implementations must reject.

Backward-compatibility behavior SHOULD be explicitly tested when consumers may depend on it.

### 10. E2E tests

E2E tests SHOULD verify critical user or system journeys through the assembled system.

E2E tests MUST NOT be treated as a replacement for:

- unit tests;
- integration tests;
- contract tests.

E2E pass rate and source-code coverage are different measurements.

Running E2E tests MUST NOT be interpreted as source-code coverage unless explicit coverage instrumentation exists.

Prefer a small set of high-value E2E scenarios over exhaustive duplication of lower-level tests.

### 11. Security-related testing

Security-sensitive changes SHOULD include tests for relevant security properties.

Examples include:

- authorization boundaries;
- rejected unauthorized operations;
- privilege escalation paths;
- validation of untrusted input;
- secret handling;
- unsafe deserialization;
- path traversal;
- injection boundaries;
- security-sensitive defaults.

Language-specific security scanners and dependency audits SHOULD be run when available and appropriate.

Static analysis and dependency scanning supplement behavioral testing.

They do not replace it.

### 12. Standards

Testing processes SHOULD follow risk-oriented testing principles consistent with recognized software-testing practices
such as ISO/IEC/IEEE 29119.

Where appropriate, retain enough information to identify:

- test basis;
- scope;
- test cases;
- test data;
- results;
- discovered defects.

Do not claim formal standards certification or conformity unless such assessment has actually been performed.

### 13. Repository-specific configuration

This skill intentionally does not define:

- test command names;
- package-manager commands;
- CI job names;
- repository paths;
- language-specific frameworks;
- coverage collector implementations;
- container images;
- test database products;
- baseline coverage values for individual packages;
- explicit lists of core files or packages.

These belong in repository-specific documentation or configuration.

A repository SHOULD document:

- supported test commands;
- coverage collection commands;
- coverage report locations;
- configured test frameworks;
- language-specific coverage tools;
- known coverage exclusions;
- baseline debt;
- explicitly designated core components;
- integration-test dependencies;
- E2E tooling;
- contract locations and formats.

Repository-specific rules MAY strengthen this skill.

They SHOULD NOT weaken MUST-level requirements concerning production-data isolation, deterministic testing, or
correctness of coverage reporting.

### 14. Final verification

Before considering testing complete, verify that:

- affected observable behavior is tested;
- important branches are tested;
- important failure paths are tested;
- relevant boundaries are considered;
- state transitions are covered where applicable;
- public and cross-component contracts remain valid;
- core code receives the stronger coverage requirement;
- tests are deterministic and independently executable;
- production persistent state is never used;
- coverage reports were actually generated;
- applicable coverage gates pass;
- meaningful tests were not removed solely because they added no coverage;
- E2E results are not confused with source-code coverage;
- applicable security validation has been performed.