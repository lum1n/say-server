"""Python SDK for say.

The client will be generated from protocol/openapi.yaml. Until then this
package only pins the shared enums.
"""

PRINCIPAL_KINDS = ("human", "agent", "guest", "ghost", "system")
ROOM_KINDS = ("dm", "group", "channel", "widget-thread")
EVENT_TYPES = (
    "message",
    "edit",
    "reaction",
    "typing",
    "receipt",
    "agent-settings",
    "handoff",
)
