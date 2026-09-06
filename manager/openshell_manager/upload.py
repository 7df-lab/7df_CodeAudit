"""Streaming multipart/form-data parser (stdlib only).

The manager's other endpoints read a whole JSON body into memory; file
uploads cannot afford that. This parser consumes the request body
incrementally from ``rfile`` (64KiB reads) and yields text fields as bytes
plus the file part as a lazy byte stream, so the HTTP layer can forward
each completed 2MiB chunk into the sandbox before the rest of the request
has even arrived. Memory stays constant regardless of file size.

A delimiter token may straddle read boundaries, so payload emission always
withholds a ``len(delimiter) - 1`` byte lookback tail until the next read
confirms those bytes are not the start of a boundary.
"""
from __future__ import annotations

from typing import BinaryIO, Dict, Iterator, Tuple

READ_CHUNK = 64 * 1024


class UploadError(Exception):
    """Malformed multipart body or oversized field (maps to HTTP 400)."""


def boundary_from_content_type(content_type: str) -> bytes:
    """Extract the boundary token from a multipart/form-data Content-Type."""
    for param in content_type.split(";")[1:]:
        key, _, value = param.strip().partition("=")
        if key.lower() == "boundary" and value:
            return value.strip('"').encode("latin-1")
    raise UploadError("multipart Content-Type missing boundary parameter")


class StreamingMultipartParser:
    """Parse once up front (``parse()``), then drain the file stream.

    ``parse()`` returns ``(fields, file_stream)`` where ``fields`` maps form
    field names to raw bytes and ``file_stream`` is a generator yielding the
    ``file`` part's payload in order. Draining ``file_stream`` to exhaustion
    validates the closing boundary; abandoning it early leaves the request
    body partially unread (the HTTP layer closes the connection then).
    """

    def __init__(self, stream: BinaryIO, boundary: bytes,
                 max_field_bytes: int = 64 * 1024):
        self._stream = stream
        self._boundary = boundary
        self._delimiter = b"\r\n--" + boundary
        self._max_field_bytes = max_field_bytes
        self._buffer = b""
        self._read_total = 0

    @property
    def bytes_consumed(self) -> int:
        """Body bytes consumed so far (read minus still-buffered). The HTTP
        layer uses this to drain the unread remainder of a Content-Length
        body so the keep-alive connection stays usable."""
        return self._read_total - len(self._buffer)

    def parse(self) -> Tuple[Dict[str, bytes], Iterator[bytes]]:
        fields: Dict[str, bytes] = {}
        saw_file = False
        first_part = True
        while True:
            if first_part:
                self._expect_first_boundary()
                first_part = False
            else:
                self._expect_next_part()
            headers = self._read_headers()
            name = self._part_name(headers)
            disposition = headers.get("content-disposition", "")
            if "filename" in disposition and name == "file":
                saw_file = True
                break
            if name is not None:
                fields[name.encode("utf-8")] = self._read_field_payload()
            else:
                self._read_field_payload()  # unnamed part: skip
        if not saw_file:
            raise UploadError("multipart body missing file part 'file'")
        return fields, self._file_stream()

    # -- internals -----------------------------------------------------------

    def _fill(self) -> bool:
        """Read one more chunk; False on end of stream."""
        more = self._stream.read(READ_CHUNK)
        if not more:
            return False
        self._read_total += len(more)
        self._buffer += more
        return True

    def _expect_first_boundary(self) -> None:
        """Consume the opening boundary line (``--boundary CRLF``)."""
        token = b"--" + self._boundary
        while True:
            pos = self._buffer.find(token)
            if pos != -1:
                break
            if not self._fill():
                raise UploadError("unexpected end of multipart body")
        if pos != 0:
            raise UploadError("garbage before multipart boundary")
        self._buffer = self._buffer[len(token):]
        self._strip_boundary_terminator()

    def _expect_next_part(self) -> None:
        """Advance past the delimiter consumed by the previous payload read.

        ``_read_field_payload`` consumed through ``CRLF--boundary``; the
        buffer now starts at the boundary-line terminator, which is either
        ``CRLF`` (another part follows) or ``--`` (end of body).
        """
        self._strip_boundary_terminator()

    def _strip_boundary_terminator(self) -> None:
        """After a boundary token: part start is CRLF, body end is ``--``."""
        while len(self._buffer) < 2:
            if not self._fill():
                raise UploadError("unexpected end of multipart body")
        if self._buffer.startswith(b"--"):
            raise UploadError("multipart body ended before file part")
        if not self._buffer.startswith(b"\r\n"):
            raise UploadError("malformed boundary delimiter")

    def _read_headers(self) -> Dict[str, str]:
        self._buffer = self._buffer[2:]  # CRLF after boundary line
        headers: Dict[str, str] = {}
        while b"\r\n\r\n" not in self._buffer:
            if not self._fill():
                raise UploadError("unexpected end of part headers")
        block, self._buffer = self._buffer.split(b"\r\n\r\n", 1)
        for line in block.split(b"\r\n"):
            text = line.decode("utf-8", errors="replace")
            key, _, value = text.partition(":")
            headers[key.strip().lower()] = value.strip()
        return headers

    @staticmethod
    def _part_name(headers: Dict[str, str]) -> "str | None":
        disposition = headers.get("content-disposition", "")
        for param in disposition.split(";")[1:]:
            key, _, value = param.strip().partition("=")
            if key.lower() == "name" and value:
                return value.strip().strip('"')
        return None

    def _read_field_payload(self) -> bytes:
        """Consume a field's payload and its delimiter (``CRLF--boundary``).

        On return the buffer starts at the byte after the boundary token,
        ready for ``_expect_next_part``.
        """
        keep = len(self._delimiter) - 1  # lookback for a split delimiter
        chunks: list[bytes] = []
        total = 0
        while True:
            pos = self._buffer.find(self._delimiter)
            if pos != -1:
                chunks.append(self._buffer[:pos])
                self._buffer = self._buffer[pos + len(self._delimiter):]
                data = b"".join(chunks)
                if len(data) > self._max_field_bytes:
                    raise UploadError(
                        f"form field exceeds {self._max_field_bytes} bytes")
                return data
            # No delimiter yet: all but a split-delimiter lookback tail is
            # payload.
            if len(self._buffer) > keep:
                chunks.append(self._buffer[:-keep])
                total += len(self._buffer) - keep
                self._buffer = self._buffer[-keep:]
            if total > self._max_field_bytes:
                raise UploadError(
                    f"form field exceeds {self._max_field_bytes} bytes")
            if not self._fill():
                raise UploadError("unexpected end of field payload")

    def _file_stream(self) -> Iterator[bytes]:
        """Yield file payload until the closing delimiter, then validate it.

        A delimiter candidate is only a real boundary when followed by
        ``--`` or CRLF; anything else (e.g. a payload-embedded
        ``CRLF--boundary-like`` byte run) is released as payload and the
        scan resumes after it.
        """
        keep = len(self._delimiter) + 2  # delimiter + its 2-byte terminator
        search_from = 0
        while True:
            pos = self._buffer.find(self._delimiter, search_from)
            if pos != -1 and len(self._buffer) >= pos + keep:
                terminator = self._buffer[pos + len(self._delimiter):
                                          pos + keep]
                if terminator.startswith(b"--"):
                    yield self._buffer[:pos]
                    self._buffer = b""
                    return
                if terminator == b"\r\n":
                    # Boundary without "--": another part follows the file.
                    # Our contract stops at the file part; leave the rest to
                    # the HTTP layer's drain.
                    yield self._buffer[:pos]
                    self._buffer = b""
                    return
                # False candidate: payload bytes, resume scanning after it.
                search_from = pos + keep
                continue
            if pos == -1:
                # No candidate at all: everything past the lookback tail is
                # releasable payload.
                if len(self._buffer) > keep:
                    yield self._buffer[:-keep]
                    self._buffer = self._buffer[-keep:]
                    search_from = 0
            else:
                # Candidate near the buffer end: need more bytes to check
                # its terminator. Release what precedes it.
                if pos > keep:
                    yield self._buffer[:pos - keep]
                    self._buffer = self._buffer[pos - keep:]
                search_from = 0
            if not self._fill():
                raise UploadError("unexpected end of file payload")
