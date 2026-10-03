# LambdaWorks Formlander patch

This repository starts from upstream [Formlander v3.2.3](https://github.com/karloscodes/formlander/tree/v3.2.3), commit `30564f929391f9fddbade0bff8da010a4738a315`. The upstream MIT licence is retained.

## SMTP greeting hostname

Google Workspace SMTP relay rejects the default `EHLO localhost` greeting in our deployment with `421 4.7.0`, then closes the connection. Go's SMTP client can surface the closure as `EOF`.

Set the following runtime environment variable to use a DNS hostname in the SMTP greeting:

```text
FORMLANDER_SMTP_HELO_HOSTNAME=formlander.lambdaworks.io
```

The hostname is set before STARTTLS and reused after the TLS upgrade. An empty variable preserves upstream's default greeting. Nonempty values must use DNS hostname syntax; invalid values fail before connecting. The greeting hostname is not the SMTP server address or the sender email address.

No change to TLS verification, relay authorisation, authentication or message recipients is required. Failures now include their SMTP phase. Credentials are not added to logs.

Regression coverage includes a relay that rejects localhost, runtime-variable mapping, backward compatibility, invalid hostname rejection and greeting failure diagnostics.

## Build and publication

The default branch is `lw-main`. The LambdaWorks workflow runs the internal Go tests with the race detector. A tag such as `lw-3.2.3-smtp-helo.1` publishes the Linux AMD64 image:

```text
ghcr.io/lambdaworks/formlander:3.2.3-smtp-helo.1
```

Pin production to the image digest recorded in the workflow summary. Publishing a new version requires a new tag; do not move release tags. The inherited upstream release workflow is not triggered by the `lw-` tag prefix.

GitHub may create a new container package as private. Verify anonymous registry access before configuring Coolify for credential-free pulls; an organisation administrator may need to set the package visibility to public.

## Deployment and rollback

1. Back up the SQLite database, uploaded files and application configuration outside this repository. Protect backups as sensitive data.
2. Verify SQLite integrity and archive checksums, and retain a copy off the application server.
3. Preserve the existing `/app/storage` volume and session secrets.
4. Configure the new runtime variable and the pinned image in Coolify.
5. Verify health, existing form/submission counts and an actual Formlander email event.

Container restarts and redeployments retain the fix when they use the patched image and persistent Coolify environment configuration. Returning to an unpatched upstream image removes the SMTP greeting fix.

A version upgrade can migrate the database. Rolling back from 3.2.3 to 3.1.5 may require restoring the pre-upgrade storage backup as well as selecting the old image. Do not overwrite newer submissions without explicitly accounting for them.

Do not commit credentials, generated databases, uploads, backups or production configuration into this repository.
