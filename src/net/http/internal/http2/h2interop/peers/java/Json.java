// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

import java.util.ArrayList;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;

// Json is a minimal JSON parser and serializer, since the JDK has none.
// Objects parse to LinkedHashMap, arrays to ArrayList, numbers to Long
// or Double, and strings, booleans and null to the obvious types.
final class Json {
    private final String s;
    private int i;

    private Json(String s) {
        this.s = s;
    }

    // parse parses a complete JSON document.
    static Object parse(String s) {
        Json p = new Json(s);
        p.ws();
        Object v = p.value();
        p.ws();
        if (p.i != s.length()) {
            throw p.err("trailing data");
        }
        return v;
    }

    private IllegalArgumentException err(String msg) {
        return new IllegalArgumentException("JSON: " + msg + " at offset " + i);
    }

    private void ws() {
        while (i < s.length()) {
            char c = s.charAt(i);
            if (c != ' ' && c != '\t' && c != '\n' && c != '\r') {
                break;
            }
            i++;
        }
    }

    private void expect(char c) {
        if (i >= s.length() || s.charAt(i) != c) {
            throw err("expected '" + c + "'");
        }
        i++;
    }

    private Object value() {
        if (i >= s.length()) {
            throw err("unexpected end");
        }
        char c = s.charAt(i);
        switch (c) {
            case '{': {
                i++;
                Map<String, Object> m = new LinkedHashMap<>();
                ws();
                if (i < s.length() && s.charAt(i) == '}') {
                    i++;
                    return m;
                }
                for (;;) {
                    ws();
                    String k = string();
                    ws();
                    expect(':');
                    ws();
                    m.put(k, value());
                    ws();
                    if (i < s.length() && s.charAt(i) == ',') {
                        i++;
                        continue;
                    }
                    expect('}');
                    return m;
                }
            }
            case '[': {
                i++;
                List<Object> l = new ArrayList<>();
                ws();
                if (i < s.length() && s.charAt(i) == ']') {
                    i++;
                    return l;
                }
                for (;;) {
                    ws();
                    l.add(value());
                    ws();
                    if (i < s.length() && s.charAt(i) == ',') {
                        i++;
                        continue;
                    }
                    expect(']');
                    return l;
                }
            }
            case '"':
                return string();
            case 't':
                lit("true");
                return Boolean.TRUE;
            case 'f':
                lit("false");
                return Boolean.FALSE;
            case 'n':
                lit("null");
                return null;
            default:
                return number();
        }
    }

    private void lit(String w) {
        if (!s.startsWith(w, i)) {
            throw err("bad literal");
        }
        i += w.length();
    }

    private Object number() {
        int start = i;
        boolean frac = false;
        while (i < s.length()) {
            char c = s.charAt(i);
            if ((c >= '0' && c <= '9') || c == '-' || c == '+') {
                i++;
            } else if (c == '.' || c == 'e' || c == 'E') {
                frac = true;
                i++;
            } else {
                break;
            }
        }
        String n = s.substring(start, i);
        if (n.isEmpty()) {
            throw err("unexpected character");
        }
        return frac ? (Object) Double.parseDouble(n) : (Object) Long.parseLong(n);
    }

    private String string() {
        expect('"');
        StringBuilder sb = new StringBuilder();
        for (;;) {
            if (i >= s.length()) {
                throw err("unterminated string");
            }
            char c = s.charAt(i++);
            if (c == '"') {
                return sb.toString();
            }
            if (c != '\\') {
                sb.append(c);
                continue;
            }
            if (i >= s.length()) {
                throw err("unterminated escape");
            }
            char e = s.charAt(i++);
            switch (e) {
                case '"': sb.append('"'); break;
                case '\\': sb.append('\\'); break;
                case '/': sb.append('/'); break;
                case 'b': sb.append('\b'); break;
                case 'f': sb.append('\f'); break;
                case 'n': sb.append('\n'); break;
                case 'r': sb.append('\r'); break;
                case 't': sb.append('\t'); break;
                case 'u':
                    if (i + 4 > s.length()) {
                        throw err("bad \\u escape");
                    }
                    sb.append((char) Integer.parseInt(s.substring(i, i + 4), 16));
                    i += 4;
                    break;
                default:
                    throw err("bad escape");
            }
        }
    }

    // toJson serializes v, which must be a Map, List, String, Number,
    // Boolean or null.
    static String toJson(Object v) {
        StringBuilder sb = new StringBuilder();
        write(sb, v);
        return sb.toString();
    }

    private static void write(StringBuilder sb, Object v) {
        if (v == null) {
            sb.append("null");
        } else if (v instanceof String str) {
            quote(sb, str);
        } else if (v instanceof Number || v instanceof Boolean) {
            sb.append(v);
        } else if (v instanceof Map<?, ?> m) {
            sb.append('{');
            boolean first = true;
            for (Map.Entry<?, ?> e : m.entrySet()) {
                if (!first) {
                    sb.append(',');
                }
                first = false;
                quote(sb, (String) e.getKey());
                sb.append(':');
                write(sb, e.getValue());
            }
            sb.append('}');
        } else if (v instanceof List<?> l) {
            sb.append('[');
            for (int j = 0; j < l.size(); j++) {
                if (j > 0) {
                    sb.append(',');
                }
                write(sb, l.get(j));
            }
            sb.append(']');
        } else {
            throw new IllegalArgumentException("JSON: can't serialize " + v.getClass());
        }
    }

    private static void quote(StringBuilder sb, String str) {
        sb.append('"');
        for (int j = 0; j < str.length(); j++) {
            char c = str.charAt(j);
            switch (c) {
                case '"': sb.append("\\\""); break;
                case '\\': sb.append("\\\\"); break;
                case '\n': sb.append("\\n"); break;
                case '\r': sb.append("\\r"); break;
                case '\t': sb.append("\\t"); break;
                default:
                    if (c < 0x20 || c == 0x7f || (Character.isSurrogate(c) && !validSurrogate(str, j))) {
                        sb.append(String.format("\\u%04x", (int) c));
                    } else {
                        sb.append(c);
                    }
            }
        }
        sb.append('"');
    }

    // validSurrogate reports whether the surrogate at str[j] is part of
    // a well-formed pair.
    private static boolean validSurrogate(String str, int j) {
        char c = str.charAt(j);
        if (Character.isHighSurrogate(c)) {
            return j + 1 < str.length() && Character.isLowSurrogate(str.charAt(j + 1));
        }
        return j > 0 && Character.isHighSurrogate(str.charAt(j - 1));
    }
}
