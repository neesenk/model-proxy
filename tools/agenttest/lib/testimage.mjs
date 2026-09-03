// 确定性测试图生成:无依赖 PNG 编码(zlib + CRC32)。
// 两张图各有可机器断言的Ground Truth:
//   quadrantPng  四象限纯色(左上红/右上绿/左下蓝/右下黄)
//   digitPng     黑底白字数字(3x5 点阵放大),默认 "42"
import zlib from "node:zlib";

const CRC_TABLE = (() => {
  const t = new Uint32Array(256);
  for (let n = 0; n < 256; n++) {
    let c = n;
    for (let k = 0; k < 8; k++) c = c & 1 ? 0xedb88320 ^ (c >>> 1) : c >>> 1;
    t[n] = c >>> 0;
  }
  return t;
})();

function crc32(buf) {
  let c = 0xffffffff;
  for (const b of buf) c = CRC_TABLE[(c ^ b) & 0xff] ^ (c >>> 8);
  return (c ^ 0xffffffff) >>> 0;
}

function chunk(type, data) {
  const len = Buffer.alloc(4);
  len.writeUInt32BE(data.length);
  const body = Buffer.concat([Buffer.from(type, "ascii"), data]);
  const crc = Buffer.alloc(4);
  crc.writeUInt32BE(crc32(body));
  return Buffer.concat([len, body, crc]);
}

export function pngEncode(width, height, rgb) {
  const ihdr = Buffer.alloc(13);
  ihdr.writeUInt32BE(width, 0);
  ihdr.writeUInt32BE(height, 4);
  ihdr[8] = 8; // bit depth
  ihdr[9] = 2; // truecolor RGB
  const raw = Buffer.alloc(height * (1 + width * 3));
  for (let y = 0; y < height; y++) {
    raw[y * (1 + width * 3)] = 0; // filter: none
    rgb.copy(raw, y * (1 + width * 3) + 1, y * width * 3, (y + 1) * width * 3);
  }
  return Buffer.concat([
    Buffer.from([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a]),
    chunk("IHDR", ihdr),
    chunk("IDAT", zlib.deflateSync(raw)),
    chunk("IEND", Buffer.alloc(0)),
  ]);
}

export const QUADRANTS = {
  topLeft: { name: "red", rgb: [220, 30, 30] },
  topRight: { name: "green", rgb: [30, 180, 30] },
  bottomLeft: { name: "blue", rgb: [30, 60, 220] },
  bottomRight: { name: "yellow", rgb: [230, 210, 20] },
};

export function quadrantPng(size = 240) {
  const rgb = Buffer.alloc(size * size * 3);
  const half = size / 2;
  for (let y = 0; y < size; y++) {
    for (let x = 0; x < size; x++) {
      const q = y < half ? (x < half ? QUADRANTS.topLeft : QUADRANTS.topRight) : x < half ? QUADRANTS.bottomLeft : QUADRANTS.bottomRight;
      rgb.set(q.rgb, (y * size + x) * 3);
    }
  }
  return pngEncode(size, size, rgb);
}

// 3x5 点阵字体(0-9)
const FONT = {
  "0": [0b111, 0b101, 0b101, 0b101, 0b111],
  "1": [0b010, 0b110, 0b010, 0b010, 0b111],
  "2": [0b111, 0b001, 0b111, 0b100, 0b111],
  "3": [0b111, 0b001, 0b111, 0b001, 0b111],
  "4": [0b101, 0b101, 0b111, 0b001, 0b001],
  "5": [0b111, 0b100, 0b111, 0b001, 0b111],
  "6": [0b111, 0b100, 0b111, 0b101, 0b111],
  "7": [0b111, 0b001, 0b010, 0b010, 0b010],
  "8": [0b111, 0b101, 0b111, 0b101, 0b111],
  "9": [0b111, 0b101, 0b111, 0b001, 0b111],
};

export function digitPng(text = "42", scale = 24) {
  const glyphCols = text.length * 4 - 1; // 3 宽 + 1 间距
  const margin = 2 * scale; // 四周留白,数字不贴边
  const width = glyphCols * scale + margin * 2;
  const height = 5 * scale + margin * 2;
  const rgb = Buffer.alloc(width * height * 3, 255); // 白底
  text.split("").forEach((ch, i) => {
    const glyph = FONT[ch];
    if (!glyph) throw new Error(`digitPng 只支持 0-9: ${ch}`);
    for (let gy = 0; gy < 5; gy++) {
      for (let gx = 0; gx < 3; gx++) {
        if (!(glyph[gy] & (0b100 >> gx))) continue;
        for (let dy = 0; dy < scale; dy++) {
          for (let dx = 0; dx < scale; dx++) {
            const x = margin + (i * 4 + gx) * scale + dx;
            const y = margin + gy * scale + dy;
            rgb.set([0, 0, 0], (y * width + x) * 3); // 黑字
          }
        }
      }
    }
  });
  return pngEncode(width, height, rgb);
}

export function quadrantImageContent() {
  return { type: "image", data: quadrantPng().toString("base64"), mimeType: "image/png" };
}
