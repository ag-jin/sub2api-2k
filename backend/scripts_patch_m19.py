import re

p = 'internal/repository/usage_log_repo_insert.go'
s = open(p).read()
assert 'upstream_credit' not in s, "已打补丁，勿重复"

starts = [m.start() for m in re.finditer(r'INSERT INTO usage_logs \(', s)]
assert len(starts) == 4, f"预期 4 个 INSERT，实际 {len(starts)}"

for st in reversed(starts):
    # 找本块的闭合括号：其后第一个非空白 token 是 SELECT/VALUES/ON CONFLICT/RETURNING
    i = st
    close = None
    while True:
        j = s.index(')', i)
        nxt = s[j+1:j+40].lstrip()
        if nxt.startswith(('SELECT', 'VALUES', 'ON CONFLICT', 'RETURNING', ';', 'AS')):
            close = j
            break
        i = j + 1
        if i - st > 8000:
            raise RuntimeError(f"块@{st} 8000 字内未找到闭合")

    seg = s[st:close]
    lines = seg.splitlines()
    idxs = [k for k, l in enumerate(lines) if l.strip() == 'created_at']
    assert idxs, f"块@{st} 列清单无 created_at"
    last = idxs[-1]
    ind = lines[last][:len(lines[last]) - len(lines[last].lstrip())]
    lines[last] = f'{ind}created_at,\n{ind}upstream_credit'

    # VALUES 形态：占位符也要补一个（$63 = 新最大值）
    if ') VALUES (' in seg:
        mx = max(int(x) for x in re.findall(r'\$(\d+)', seg))
        for k in range(len(lines)-1, -1, -1):
            if f'${mx}' in lines[k]:
                ind2 = lines[k][:len(lines[k]) - len(lines[k].lstrip())]
                lines[k] = f"{lines[k]},\n{ind2}${mx+1}"
                break

    s = s[:st] + '\n'.join(lines) + s[close:]

open(p, 'w').write(s)
print("4 个 INSERT 块全部补齐 upstream_credit")
