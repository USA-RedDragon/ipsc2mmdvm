#include <cstdio>
#include <cstring>
#include <string>
#include <vector>

static std::vector<unsigned char> g_out;

static bool capture(const unsigned char* d, unsigned int n)
{
	g_out.assign(d, d + n);
	return true;
}

namespace pre {
enum { RUNNING };
struct CDMRNetwork {
	int m_status = RUNNING;
	unsigned char* m_id;
	bool write(const unsigned char* d, unsigned int n) { return capture(d, n); }
	bool writeRadioPosition(unsigned int id, const unsigned char* data);
	bool writeTalkerAlias(unsigned int id, unsigned char type, const unsigned char* data);
};
#include "pre.inc"
}

namespace host {
static bool g_listenOnly = false;
struct CDMRNetwork {
	bool write(const unsigned char* d, unsigned int n) { return capture(d, n); }
	bool writeRadioPosition(unsigned int id, const unsigned char* data);
	bool writeTalkerAlias(unsigned int id, unsigned char type, const unsigned char* data);
};
#include "host.inc"
}

namespace gw {
enum class STATUS { RUNNING };
struct CDMRNetwork {
	STATUS m_status = STATUS::RUNNING;
	bool m_location = true;
	unsigned char m_id[4];
	bool write(const unsigned char* d, unsigned int n) { return capture(d, n); }
	bool writeRadioPosition(const unsigned char* data, unsigned int length);
	bool writeTalkerAlias(const unsigned char* data, unsigned int length);
};
#include "gw.inc"
}

static std::string hex(const std::vector<unsigned char>& v)
{
	std::string s;
	char b[3];
	for (unsigned char c : v) {
		::snprintf(b, sizeof(b), "%02x", c);
		s += b;
	}
	return s;
}

int main(int argc, char** argv)
{
	if (argc < 3)
		return 1;
	unsigned int rpt = std::stoul(argv[1]);
	unsigned int src = std::stoul(argv[2]);
	unsigned char id[4] = {(unsigned char)(rpt >> 24), (unsigned char)(rpt >> 16), (unsigned char)(rpt >> 8), (unsigned char)rpt};

	pre::CDMRNetwork p;
	p.m_id = id;
	host::CDMRNetwork h;
	gw::CDMRNetwork g;
	::memcpy(g.m_id, id, 4);

	char line[64];
	while (::fgets(line, sizeof(line), stdin) != nullptr) {
		unsigned char lc[9];
		for (int i = 0; i < 9; i++)
			::sscanf(line + 2 * i, "%2hhx", &lc[i]);
		unsigned char flco = lc[0] & 0x3FU;
		std::vector<unsigned char> a, b, c;
		if (flco >= 4U && flco <= 7U) {
			p.writeTalkerAlias(src, flco - 4U, lc); a = g_out;
			h.writeTalkerAlias(src, flco - 4U, lc); b = g_out;
			g.writeTalkerAlias(b.data(), b.size()); c = g_out;
		} else if (flco == 8U) {
			p.writeRadioPosition(src, lc); a = g_out;
			h.writeRadioPosition(src, lc); b = g_out;
			g.writeRadioPosition(b.data(), b.size()); c = g_out;
		} else {
			continue;
		}
		std::vector<unsigned char> l(lc, lc + 9);
		::printf("%s %s %s %s\n", hex(l).c_str(), hex(a).c_str(), hex(b).c_str(), hex(c).c_str());
	}
	return 0;
}
