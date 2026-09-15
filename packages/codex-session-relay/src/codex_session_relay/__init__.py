"""Durable same-host verification requests between independent Codex tasks.

The core is standard library only. Only the real host adapter needs the transport
bridge, and it is imported lazily so importing this package never requires it.
"""

__version__ = "0.1.0"

NO_DELIVERABLE = "0" * 64

__all__ = ["__version__", "NO_DELIVERABLE"]
