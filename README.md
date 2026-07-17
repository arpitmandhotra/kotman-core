# Kaughtman Core API Integrator

This repository runs the core merchant integrations, signals processing, and buyer intelligence dashboard APIs.

## Initial Setup

Copy `.env.example` to `.env` and fill in real values before running.

Before the application starts serving pincode-dependent features, you must run the one-time pincode database seed job to download and populate the India post office geographic database.

Run this command once:

```bash
go run cmd/seed/pincode_seed.go
```

This job downloads the government's official postal directory, resolves coordinates, classifies geographic tiers, and populates the local reference table.
