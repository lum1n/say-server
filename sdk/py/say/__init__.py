"""Python SDK for say.

schema_gen.py holds the API's types, generated from protocol/openapi.yaml
(`make generate-py`); Client is a thin REST client over them.
"""

from typing import get_args

from .client import Client, EventPage, SayError
from .schema_gen import *
from .schema_gen import PrincipalKind, RoomKind

PRINCIPAL_KINDS = get_args(PrincipalKind)
ROOM_KINDS = get_args(RoomKind)
