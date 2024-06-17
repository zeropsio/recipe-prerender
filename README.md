# Zerops x Prerender

Import as a service to an existing Zerops project.

```yaml
services:
  - hostname: prerender
    type: nodejs@20
    buildFromGit: https://github.com/zeropsio/recipe-prerender
    enableSubdomainAccess: true
```
