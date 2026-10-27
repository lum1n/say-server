"""A thin REST client over the types in schema_gen.py, with no dependencies."""

from __future__ import annotations

import http.client
import http.cookies
import json
import urllib.error
import urllib.parse
import urllib.request
from typing import Any, TypedDict

from .schema_gen import (
    AddMember,
    Bridge,
    BridgeLink,
    BridgeSettings,
    BuiltinAgent,
    BuiltinAgentSettings,
    CreateBridgeLink,
    CreateBuiltinAgent,
    CreatedPrincipal,
    CreatePrincipal,
    CreateRoom,
    Delivery,
    EditMessage,
    EmbedConfig,
    EmbedSession,
    EmbedSessionRequest,
    Event,
    Health,
    Login,
    Member,
    Network,
    Principal,
    Room,
    SendMessage,
    Setup,
    SetupStatus,
    UpdateMember,
    UpdateRoom,
    Widget,
    WidgetSettings,
)


class SayError(Exception):
    """SayError carries the API's error code, e.g. "invalid_credentials"."""

    def __init__(self, status: int, code: str, message: str):
        super().__init__(message)
        self.status = status
        self.code = code


class EventPage(TypedDict):
    events: list[Event]
    deliveries: list[Delivery]


def _seg(s: str) -> str:
    return urllib.parse.quote(s, safe="")


class Client:
    """Client talks to one say instance as the holder of an access token.

    base_url is the API root, like http://localhost:7290/api/v1.
    """

    def __init__(self, base_url: str, token: str | None = None, timeout: float = 90):
        self.base_url = base_url.rstrip("/")
        self.token = token
        self.timeout = timeout

    def request(
        self,
        method: str,
        path: str,
        body: Any = None,
        query: dict[str, Any] | None = None,
        headers: dict[str, str] | None = None,
    ) -> Any:
        """request sends one call and returns its decoded JSON, or None for an empty reply."""
        return self._call(method, path, body, query, headers)[0]

    def _call(
        self,
        method: str,
        path: str,
        body: Any = None,
        query: dict[str, Any] | None = None,
        headers: dict[str, str] | None = None,
    ) -> tuple[Any, http.client.HTTPMessage]:
        url = self.base_url + path
        q = {k: v for k, v in (query or {}).items() if v is not None}
        if q:
            url += "?" + urllib.parse.urlencode(q)
        req = urllib.request.Request(url, method=method, headers=dict(headers or {}))
        if self.token:
            req.add_header("Authorization", f"Bearer {self.token}")
        if body is not None:
            req.data = json.dumps(body).encode()
            req.add_header("Content-Type", "application/json")
        try:
            with urllib.request.urlopen(req, timeout=self.timeout) as res:
                data, res_headers = res.read(), res.headers
        except urllib.error.HTTPError as e:
            with e:
                raw = e.read()
            try:
                err = json.loads(raw)["error"]
                code, message = err["code"], err["message"]
            except (ValueError, KeyError, TypeError):
                code, message = "internal", e.reason or str(e.code)
            raise SayError(e.code, code, message) from None
        return (json.loads(data) if data else None), res_headers

    def _sign_in(self, path: str, body: Any) -> Principal:
        principal, headers = self._call("POST", path, body)
        cookie = http.cookies.SimpleCookie()
        for h in headers.get_all("Set-Cookie") or []:
            cookie.load(h)
        if "say_token" in cookie:
            self.token = cookie["say_token"].value
        return principal

    def get_health(self) -> Health:
        return self.request("GET", "/health")

    def get_setup(self) -> SetupStatus:
        return self.request("GET", "/setup")

    def setup(self, body: Setup) -> Principal:
        """setup creates the first admin and signs this client in as them."""
        return self._sign_in("/setup", body)

    def login(self, body: Login) -> Principal:
        """login signs this client in with the session token the server sets as a cookie."""
        return self._sign_in("/login", body)

    def logout(self) -> None:
        """logout revokes the client's token."""
        self.request("POST", "/logout")
        self.token = None

    def get_me(self) -> Principal:
        return self.request("GET", "/me")

    def list_principals(self) -> list[Principal]:
        return self.request("GET", "/principals")["principals"]

    def create_principal(self, body: CreatePrincipal) -> CreatedPrincipal:
        """create_principal adds a person or an agent; an agent's token is only returned here."""
        return self.request("POST", "/principals", body)

    def list_rooms(self) -> list[Room]:
        return self.request("GET", "/rooms")["rooms"]

    def create_room(self, body: CreateRoom) -> Room:
        return self.request("POST", "/rooms", body)

    def get_room(self, id: str) -> Room:
        return self.request("GET", f"/rooms/{_seg(id)}")

    def update_room(self, id: str, body: UpdateRoom) -> Room:
        """update_room changes a room's agent settings."""
        return self.request("PATCH", f"/rooms/{_seg(id)}", body)

    def list_members(self, id: str) -> list[Member]:
        return self.request("GET", f"/rooms/{_seg(id)}/members")["members"]

    def add_member(self, id: str, body: AddMember) -> Member:
        return self.request("POST", f"/rooms/{_seg(id)}/members", body)

    def update_member(self, id: str, principal_id: str, body: UpdateMember) -> Member:
        """update_member makes an agent a designated responder in a room, or stops it being one."""
        return self.request("PATCH", f"/rooms/{_seg(id)}/members/{_seg(principal_id)}", body)

    def list_builtin_agents(self) -> list[BuiltinAgent]:
        return self.request("GET", "/builtin-agents")["agents"]

    def create_builtin_agent(self, body: CreateBuiltinAgent) -> BuiltinAgent:
        return self.request("POST", "/builtin-agents", body)

    def update_builtin_agent(self, id: str, body: BuiltinAgentSettings) -> BuiltinAgent:
        """update_builtin_agent changes the fields given; an absent api_key keeps the stored one."""
        return self.request("PATCH", f"/builtin-agents/{_seg(id)}", body)

    def list_events(
        self,
        id: str,
        after: int | None = None,
        before: int | None = None,
        limit: int | None = None,
        wait: int | None = None,
    ) -> list[Event]:
        """list_events reads a page of a room's log in seq order: forward from
        `after`, or else the page that ends before `before`, or the latest page.
        """
        return self.list_event_page(id, after, before, limit, wait)["events"]

    def list_event_page(
        self,
        id: str,
        after: int | None = None,
        before: int | None = None,
        limit: int | None = None,
        wait: int | None = None,
    ) -> EventPage:
        """list_event_page is list_events with how the page's messages went out to the room's bridges."""
        query = {"after": after, "before": before, "limit": limit, "wait": wait}
        return self.request("GET", f"/rooms/{_seg(id)}/events", query=query)

    def send_message(self, id: str, body: SendMessage, idempotency_key: str) -> Event:
        """send_message posts a message. Retrying with the same idempotency_key is
        safe: the server returns the event the first attempt created.
        """
        headers = {"Idempotency-Key": idempotency_key}
        return self.request("POST", f"/rooms/{_seg(id)}/messages", body, headers=headers)

    def edit_message(self, id: str, event_id: str, body: EditMessage) -> Event:
        """edit_message changes one of your messages. While it streams, parts
        replace it in place; `stream: "final"` ends the stream, and a later edit
        of a final message is logged and marks it edited. It returns the message
        as it now is.
        """
        return self.request("PATCH", f"/rooms/{_seg(id)}/messages/{_seg(event_id)}", body)

    def handoff_room(self, id: str) -> None:
        """handoff_room hands a widget thread from its agent to the people in it."""
        self.request("POST", f"/rooms/{_seg(id)}/handoff")

    def list_widgets(self) -> list[Widget]:
        return self.request("GET", "/widgets")["widgets"]

    def create_widget(self, body: WidgetSettings) -> Widget:
        return self.request("POST", "/widgets", body)

    def update_widget(self, id: str, body: WidgetSettings) -> Widget:
        """update_widget changes the fields given; rotate_secret replaces the secret."""
        return self.request("PATCH", f"/widgets/{_seg(id)}", body)

    def get_embed_config(self, key: str) -> EmbedConfig:
        """get_embed_config reads a widget's public settings by its public key."""
        return self.request("GET", f"/embed/{_seg(key)}")

    def create_embed_session(self, key: str, body: EmbedSessionRequest) -> EmbedSession:
        """create_embed_session signs a widget visitor in. Use a client made with
        the visitor's earlier token to get that visitor back.
        """
        return self.request("POST", f"/embed/{_seg(key)}/session", body)

    def list_networks(self) -> list[Network]:
        return self.request("GET", "/networks")["networks"]

    def list_bridges(self) -> list[Bridge]:
        """list_bridges returns the caller's accounts, and relay bots for admins."""
        return self.request("GET", "/bridges")["bridges"]

    def create_bridge(self, body: BridgeSettings) -> Bridge:
        return self.request("POST", "/bridges", body)

    def get_bridge(self, id: str) -> Bridge:
        return self.request("GET", f"/bridges/{_seg(id)}")

    def update_bridge(self, id: str, body: BridgeSettings) -> Bridge:
        """update_bridge changes the fields given; secrets left out keep their value."""
        return self.request("PATCH", f"/bridges/{_seg(id)}", body)

    def answer_bridge(self, id: str, value: str) -> None:
        """answer_bridge answers a bridge's login prompt."""
        self.request("POST", f"/bridges/{_seg(id)}/answer", {"value": value})

    def rotate_hook_secret(self, id: str) -> dict[str, Any]:
        """rotate_hook_secret sets a new random secret for the bridge's hook and returns it with the bridge."""
        return self.request("POST", f"/bridges/{_seg(id)}/hook-secret")

    def list_bridge_links(self, id: str) -> list[BridgeLink]:
        return self.request("GET", f"/bridges/{_seg(id)}/links")["links"]

    def create_bridge_link(self, id: str, body: CreateBridgeLink) -> BridgeLink:
        return self.request("POST", f"/bridges/{_seg(id)}/links", body)

    def delete_bridge_link(self, id: str, link_id: str) -> None:
        self.request("DELETE", f"/bridges/{_seg(id)}/links/{_seg(link_id)}")

