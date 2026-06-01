"""Typed parameter dataclass with automatic runq.params merging.

``@runq.dataclass`` wraps a plain class into a ``dataclasses.dataclass``
with extras:

- **Pre-check**: all fields must have defaults — caught at class
  definition time, not at runtime.
- **Auto-overwrite**: when ``auto_overwrite=True``, matching keys from
  ``runq.params`` override defaults at instantiation time.
- **Serialization**: ``to_dict`` / ``to_json`` / ``from_json`` /
  ``to_yaml`` / ``from_yaml`` out of the box.
"""

from __future__ import annotations

import dataclasses
import json
from pathlib import Path
from typing import Any


def dataclass(
    cls=None,
    *,
    auto_overwrite: bool = False,
    strict: bool = False,
):
    """Decorator that turns a class into a runq parameter dataclass.

    All fields must have defaults. If ``auto_overwrite`` is True, matching
    keys from ``runq.params`` override defaults at instantiation time.

    Usage::

        @runq.dataclass(auto_overwrite=True)
        class MyConfig:
            lr: float = 0.001
            batch_size: int = 32

        cfg = MyConfig()       # lr overridden from runq.params if present
        cfg.to_json("cfg.json")
    """

    def wrap(cls):
        # Make it a dataclass if not already
        if not dataclasses.is_dataclass(cls):
            cls = dataclasses.dataclass(cls)

        # Pre-check: every field must have a default
        for f in dataclasses.fields(cls):
            has_default = (
                f.default is not dataclasses.MISSING
                or f.default_factory is not dataclasses.MISSING
            )
            if not has_default:
                raise TypeError(
                    f"@runq.dataclass: field '{f.name}' has no default. "
                    "All fields must have defaults so missing params are "
                    "caught at definition time, not at runtime."
                )

        # Store options on the class for introspection
        cls._runq_auto_overwrite = auto_overwrite
        cls._runq_strict = strict

        # Wrap __init__ for auto_overwrite
        if auto_overwrite:
            original_init = cls.__init__

            def new_init(self, *args, **kwargs):
                original_init(self, *args, **kwargs)
                _merge_params(self, strict)

            cls.__init__ = new_init

        # Add serialization methods
        cls.to_dict = _to_dict
        cls.to_json = _to_json
        cls.from_json = classmethod(_from_json)
        cls.to_yaml = _to_yaml
        cls.from_yaml = classmethod(_from_yaml)

        return cls

    if cls is None:
        return wrap
    return wrap(cls)


def _merge_params(instance: Any, strict: bool) -> None:
    """Merge runq.params into the dataclass instance."""
    from runq._context import get_ctx

    try:
        ctx = get_ctx()
    except RuntimeError:
        return  # no context initialized yet

    fields = {f.name for f in dataclasses.fields(instance)}
    for key, val in ctx.params.items():
        if key in fields:
            setattr(instance, key, val)
        elif strict:
            raise AttributeError(
                f"Param '{key}' from runq.params not found in "
                f"{type(instance).__name__}. "
                f"Available fields: {sorted(fields)}. "
                f"Set strict=False to ignore."
            )
        else:
            continue


def _to_dict(self) -> dict[str, Any]:
    """Convert to plain dict."""
    return {f.name: getattr(self, f.name) for f in dataclasses.fields(self)}


def _to_json(self, path: str | Path | None = None, **kwargs) -> str:
    """Serialize to JSON string. Optionally write to file."""
    data = self.to_dict()
    text = json.dumps(data, indent=2, ensure_ascii=False, **kwargs)
    if path is not None:
        Path(path).write_text(text, encoding="utf-8")
    return text


def _from_json(cls, path: str | Path) -> Any:
    """Load from a JSON file."""
    data = json.loads(Path(path).read_text(encoding="utf-8"))
    known = {f.name for f in dataclasses.fields(cls)}
    return cls(**{k: v for k, v in data.items() if k in known})


def _to_yaml(self, path: str | Path | None = None) -> str:
    """Serialize to YAML string. Requires pyyaml."""
    import yaml

    data = self.to_dict()
    text = yaml.dump(data, default_flow_style=False, allow_unicode=True)
    if path is not None:
        Path(path).write_text(text, encoding="utf-8")
    return text


def _from_yaml(cls, path: str | Path) -> Any:
    """Load from a YAML file. Requires pyyaml."""
    import yaml

    data = yaml.safe_load(Path(path).read_text(encoding="utf-8"))
    known = {f.name for f in dataclasses.fields(cls)}
    return cls(**{k: v for k, v in data.items() if k in known})
