# Security

For a suspected vulnerability, use the repository's **Security → Report a
vulnerability** option if private reporting is enabled. Otherwise, open an issue
asking for a private contact without publishing exploit details or sensitive files.
Maintainers should enable private vulnerability reporting when publishing on GitHub.

This tool reads the entire input and decodes images in memory. It does not impose
input-size, pixel-count, CPU, or memory limits. Applications processing untrusted
uploads should enforce resource limits and isolate processing. Re-encoded images
may lose metadata; compression is not a reliable metadata sanitization tool.
