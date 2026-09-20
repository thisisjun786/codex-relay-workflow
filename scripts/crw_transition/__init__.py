"""The manual-to-plugin transition, and the disable and removal that follow it.

Separate from crw_runtime on purpose. That package owns installing a runtime and diagnosing one,
and another open change owns its diagnosis output; this one owns where an installation's records
live and which owner they name. Everything here reads crw_runtime rather than reimplementing it.
"""

TRANSITION_VERSION = 1
