# Contributing to cloud-provider-gdc

We welcome contributions to the Google Distributed Cloud (GDC) air-gapped cloud-provider! Whether you are reporting a bug, proposing a feature, or submitting a Pull Request (PR), this guide will help you get started.

---

## Code of Conduct

This project follows [Google's Open Source Community Guidelines](https://opensource.google/conduct/). We expect all contributors to adhere to these standards to maintain a welcoming and inclusive environment.

---

## Contributor License Agreements

Before we can accept and merge your code or documentation contributions, you must sign a **Contributor License Agreement (CLA)**. This protects both you and Google's open-source intellectual property.

*   **Individual CLA:** If you are contributing as an individual, sign the [Individual CLA](https://developers.google.com/open-source/cla/individual).
*   **Corporate CLA:** If you are contributing on behalf of your company (such as SAP), your organization must sign the [Corporate CLA](https://developers.google.com/open-source/cla/corporate).

**Note:** Once you submit a Pull Request, an automated `@googlebot` will check your signature status. If you haven't signed yet, the bot will leave a comment on the PR with instructions on how to complete it.

---

## Local Development & Testing

To ensure your contributions build and test cleanly before submitting, run the following make targets:

```bash
make format
make check
make unittests
make build-local
```