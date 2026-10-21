"""python3 web_caller.py <api.ts> <path-fragment>: exit 0 when an exported function of the file, with its comments
removed, sends a GET whose path contains the fragment. Used by regrade.sh, which runs it outside the sandbox."""
import re
import sys


def strip_comments(src):
    """TypeScript without // and /* */ comments; string and template literals are kept as they are."""
    out, i, quote = [], 0, None
    while i < len(src):
        c = src[i]
        if quote:
            if c == "\\":
                out.append(src[i:i + 2])
                i += 2
                continue
            if c == quote:
                quote = None
        elif src.startswith("//", i):
            i = src.find("\n", i) if "\n" in src[i:] else len(src)
            continue
        elif src.startswith("/*", i):
            end = src.find("*/", i + 2)
            i = len(src) if end < 0 else end + 2
            continue
        elif c in "\"'`":
            quote = c
        out.append(c)
        i += 1
    return "".join(out)


def main():
    path, fragment = sys.argv[1], sys.argv[2]
    code = strip_comments(open(path, encoding="utf-8").read())
    starts = [m.start() for m in re.finditer(r"(?m)^export\s+(?:async\s+function|function|const)\s+\w+", code)]
    for a, b in zip(starts, starts[1:] + [len(code)]):
        body = code[a:b]
        if re.search(r"""["']GET["']""", body) and re.search(r"[`'\"][^`'\"]*" + re.escape(fragment), body):
            print(body.splitlines()[0])
            return 0
    print(f"no exported function in {path} sends GET …{fragment} (comments ignored)")
    return 1


if __name__ == "__main__":
    sys.exit(main())
