# OpenAI-Compatible Example

This example calls an OpenAI-compatible Chat Completions service selected by `OPENAI_COMPAT_BASE_URL`.

Required environment variables:

- `OPENAI_COMPAT_BASE_URL`
- `OPENAI_COMPAT_MODEL`

Optional environment variable:

- `OPENAI_COMPAT_API_KEY`

If the required values are missing, the example prints a skip message and exits successfully. Tools still execute locally under policy after the model requests them.

Run it with:

```bash
OPENAI_COMPAT_BASE_URL=http://localhost:11434/v1 OPENAI_COMPAT_MODEL=your-model go run ./examples/openai-compatible
```
