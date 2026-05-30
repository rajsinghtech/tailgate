# JSON Schemas for Tailgate CRDs

These JSON schemas enable editor validation and autocompletion for Tailgate custom resources.

## Usage

### VS Code with YAML Extension

Add a schema comment at the top of your manifest:

```yaml
# yaml-language-server: $schema=https://raw.githubusercontent.com/rajsinghtech/tailgate/main/schemas/egressgroup_v1alpha1.json
apiVersion: tailscale.rajsingh.info/v1alpha1
kind: EgressGroup
metadata:
  name: my-egress
spec:
  # You'll get autocompletion and validation here
```

### Available Schemas

| CRD | Schema URL |
|-----|------------|
| EgressGroup v1alpha1 | `https://raw.githubusercontent.com/rajsinghtech/tailgate/main/schemas/egressgroup_v1alpha1.json` |

### kubeconform Validation

Validate manifests locally with [kubeconform](https://github.com/yannh/kubeconform):

```bash
kubeconform -strict -summary \
  -schema-location default \
  -schema-location 'schemas/{{ .ResourceKind }}_{{ .ResourceAPIVersion }}.json' \
  your-manifests.yaml
```

With remote schemas:

```bash
kubeconform -strict -summary \
  -schema-location default \
  -schema-location 'https://raw.githubusercontent.com/rajsinghtech/tailgate/main/schemas/{{ .ResourceKind }}_{{ .ResourceAPIVersion }}.json' \
  your-manifests.yaml
```

## Regenerating Schemas

To regenerate schemas after CRD changes:

```bash
hack/generate-schemas.sh
```

Requires Python 3 with PyYAML (auto-installed if missing).
