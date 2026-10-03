using System.Buffers;
using System.Globalization;
using System.Net.Http.Headers;
using System.Reflection;
using System.Security.Cryptography.X509Certificates;
using System.Text;
using System.Text.Json;
using System.Text.RegularExpressions;
using Microsoft.AspNetCore.SignalR;
using Microsoft.Extensions.ML;
using Microsoft.Extensions.Options;
using Microsoft.ML.Data;
using MQTTnet;
using prediction.Server.Hubs;
using Prediction_Server;

namespace prediction.Server.Services;

public class IotHubOptions
{
    public bool Enabled { get; set; } = true;
    public string Host { get; set; } = "localhost";
    public int Port { get; set; } = 1883;
    /// <summary>MQTT over TLS (hub -mqtts, usually port 8883). The certificate is
    /// validated against the OS trust store; for a self-signed hub, install its
    /// CA certificate there.</summary>
    public bool UseTls { get; set; }
    /// <summary>Check certificate revocation online. Off by default, like HttpClient:
    /// private CAs usually publish no revocation list, which would fail every connect.</summary>
    public bool CheckCertificateRevocation { get; set; }
    /// <summary>Hub credential: MQTT username = this id, password = Token. Create it on
    /// the hub as role "service" with sensors ["*-ml"].</summary>
    public string Username { get; set; } = "mlnet-bridge";
    /// <summary>Hub token; sent as MQTT password and HTTP bearer.</summary>
    public string? Token { get; set; }
    public string TopicPrefix { get; set; } = "iot";
    /// <summary>Results go to {prefix}/{sensor}{OutputSuffix}.</summary>
    public string OutputSuffix { get; set; } = "-ml";
    /// <summary>A message is scored only if it carries all of these fields.</summary>
    public string[] RequiredFields { get; set; } = ["AccessMethod", "AccessStatus", "LocationID"];
    /// <summary>The label that means "nothing wrong"; risk = 1 - P(NormalLabel).</summary>
    public string NormalLabel { get; set; } = "Normal";
    /// <summary>Risk above this opens an anomaly episode in the hub.</summary>
    public double RiskThreshold { get; set; } = 0.5;
    /// <summary>Hub HTTP base URL, used once per sensor to register the result
    /// sensor with units and the risk rule. Empty skips registration.</summary>
    public string? HubUrl { get; set; } = "http://localhost:8080";
}

/// <summary>
/// Scores access-control events from the IoT hub with the ML.NET model and
/// publishes the result back as a sensor, so predictions get the hub's
/// charts, storage and anomaly alerts with no hub-side code.
///
///   iot/access-1      {"AccessMethod":"PIN Code","LocationID":"Back Door",...}
///   iot/access-1-ml   {"prediction":"Normal","confidence":0.97,"risk":0.03,"p_Normal":0.97,...}
/// </summary>
public sealed partial class IotHubBridge(
    IOptions<IotHubOptions> options,
    PredictionEnginePool<PredictionModel.ModelInput, PredictionModel.ModelOutput> pool,
    IHubContext<AccessControlHub> signalR,
    IHttpClientFactory httpFactory,
    ILogger<IotHubBridge> log) : BackgroundService
{
    private readonly IotHubOptions _o = options.Value;
    private readonly HashSet<string> _registered = [];
    private string[] _labels = [];
    private IMqttClient? _client;

    // ModelInput setters, resolved once. Payload keys match case-insensitively.
    private static readonly Dictionary<string, PropertyInfo> Props =
        typeof(PredictionModel.ModelInput).GetProperties()
            .ToDictionary(p => p.Name, StringComparer.OrdinalIgnoreCase);

    public long Scored { get; private set; }

    protected override async Task ExecuteAsync(CancellationToken stop)
    {
        if (!_o.Enabled)
        {
            log.LogInformation("IoT hub bridge disabled");
            return;
        }
        _labels = ReadLabels();
        log.LogInformation("IoT hub bridge: model classes [{Labels}], normal = {Normal}",
            string.Join(", ", _labels), _o.NormalLabel);
        if (!_labels.Contains(_o.NormalLabel))
            log.LogWarning("NormalLabel {Normal} is not one of the model's classes; risk will always be 1", _o.NormalLabel);

        _client = new MqttClientFactory().CreateMqttClient();
        _client.ApplicationMessageReceivedAsync += e => OnMessage(e.ApplicationMessage, stop);

        var builder = new MqttClientOptionsBuilder()
            .WithTcpServer(_o.Host, _o.Port)
            .WithClientId($"mlnet-bridge-{Environment.MachineName}")
            .WithCleanSession(true);
        if (_o.UseTls)
            builder = builder.WithTlsOptions(t => t.UseTls().WithRevocationMode(
                _o.CheckCertificateRevocation ? X509RevocationMode.Online : X509RevocationMode.NoCheck));
        // An empty password with the password flag set is a protocol
        // violation, so credentials are sent only when a token is configured.
        if (!string.IsNullOrEmpty(_o.Token))
            builder = builder.WithCredentials(_o.Username, _o.Token);
        var mqttOptions = builder.Build();

        // Connect, and reconnect with capped backoff; quiet while the hub is down.
        var delay = TimeSpan.FromSeconds(1);
        var warned = false;
        while (!stop.IsCancellationRequested)
        {
            if (!_client.IsConnected)
            {
                try
                {
                    await _client.ConnectAsync(mqttOptions, stop);
                    var sub = new MqttClientSubscribeOptionsBuilder()
                        .WithTopicFilter($"{_o.TopicPrefix}/+")
                        .Build();
                    await _client.SubscribeAsync(sub, stop);
                    log.LogInformation("IoT hub bridge connected to {Host}:{Port}, scoring {Prefix}/+", _o.Host, _o.Port, _o.TopicPrefix);
                    delay = TimeSpan.FromSeconds(1);
                    warned = false;
                }
                catch (Exception ex) when (!stop.IsCancellationRequested)
                {
                    if (!warned)
                        log.LogWarning("IoT hub not reachable at {Host}:{Port} ({Error}); retrying", _o.Host, _o.Port, ex.Message);
                    warned = true;
                    delay = TimeSpan.FromSeconds(Math.Min(delay.TotalSeconds * 2, 60));
                }
            }
            try { await Task.Delay(delay, stop); } catch (OperationCanceledException) { }
        }
        if (_client.IsConnected)
            await _client.DisconnectAsync();
        _client.Dispose();
    }

    private string[] ReadLabels()
    {
        var engine = pool.GetPredictionEngine();
        try
        {
            var col = engine.OutputSchema.GetColumnOrNull("Class")
                ?? throw new InvalidOperationException("model has no Class column");
            var keys = new VBuffer<ReadOnlyMemory<char>>();
            col.GetKeyValues(ref keys);
            return keys.DenseValues().Select(k => k.ToString()).ToArray();
        }
        finally
        {
            pool.ReturnPredictionEngine(engine);
        }
    }

    private async Task OnMessage(MqttApplicationMessage msg, CancellationToken stop)
    {
        var topic = msg.Topic;
        var sensor = topic[(topic.IndexOf('/') + 1)..];
        if (sensor.EndsWith(_o.OutputSuffix, StringComparison.Ordinal))
            return; // our own output

        JsonElement root;
        try
        {
            using var doc = JsonDocument.Parse(msg.Payload.ToArray());
            root = doc.RootElement.Clone();
        }
        catch (JsonException)
        {
            return; // bare values and non-JSON are not access events
        }
        if (root.ValueKind != JsonValueKind.Object || !_o.RequiredFields.All(f => Has(root, f)))
            return;

        long ts = root.TryGetProperty("ts", out var t) && t.TryGetInt64(out var v) ? v : DateTimeOffset.UtcNow.ToUnixTimeMilliseconds();
        var input = ToModelInput(root, ts);
        var output = pool.Predict(input);

        var payload = new Dictionary<string, object> { ["ts"] = ts, ["prediction"] = output.PredictedLabel ?? "" };
        double pNormal = 0, best = 0;
        for (var i = 0; i < _labels.Length && i < output.Score.Length; i++)
        {
            payload["p_" + SafeField(_labels[i])] = Math.Round(output.Score[i], 4);
            best = Math.Max(best, output.Score[i]);
            if (_labels[i] == _o.NormalLabel) pNormal = output.Score[i];
        }
        payload["confidence"] = Math.Round(best, 4);
        payload["risk"] = Math.Round(1 - pNormal, 4);

        var outSensor = sensor + _o.OutputSuffix;
        await RegisterOnce(outSensor, sensor, stop);
        var json = JsonSerializer.SerializeToUtf8Bytes(payload);
        await _client!.PublishAsync(new MqttApplicationMessageBuilder()
            .WithTopic($"{_o.TopicPrefix}/{outSensor}")
            .WithPayload(json)
            .Build(), stop);
        Scored++;

        // Existing SignalR clients keep receiving predictions as before.
        await signalR.Clients.All.SendAsync("Prediction", Encoding.UTF8.GetString(json), stop);
    }

    private static bool Has(JsonElement o, string name)
    {
        foreach (var p in o.EnumerateObject())
            if (string.Equals(p.Name, name, StringComparison.OrdinalIgnoreCase)) return true;
        return false;
    }

    /// <summary>
    /// Builds the model input from a hub reading. Missing numbers become NaN,
    /// which the model's ReplaceMissingValues step imputes. Timestamp, hour
    /// and weekday are derived from ts when absent (the hub turns a
    /// "Timestamp" field into the reading time).
    /// </summary>
    internal static PredictionModel.ModelInput ToModelInput(JsonElement root, long ts)
    {
        var input = new PredictionModel.ModelInput
        {
            Timestamp = "", UserID = "", LocationID = "", AccessMethod = "", AccessStatus = "", DayOfWeek = "",
            IsHoliday = "", DoorSensorStatus = "", Class = "",
            HourOfDay = float.NaN, VibrationSensorReading = float.NaN, Temperature = float.NaN, Humidity = float.NaN,
        };
        foreach (var p in root.EnumerateObject())
        {
            if (!Props.TryGetValue(p.Name, out var prop)) continue;
            if (prop.PropertyType == typeof(float))
            {
                if (p.Value.ValueKind == JsonValueKind.Number) prop.SetValue(input, p.Value.GetSingle());
                else if (p.Value.ValueKind == JsonValueKind.String &&
                         float.TryParse(p.Value.GetString(), NumberStyles.Float, CultureInfo.InvariantCulture, out var f))
                    prop.SetValue(input, f);
            }
            else if (prop.PropertyType == typeof(string))
            {
                prop.SetValue(input, p.Value.ValueKind switch
                {
                    JsonValueKind.String => p.Value.GetString(),
                    JsonValueKind.True => "True",
                    JsonValueKind.False => "False",
                    _ => p.Value.GetRawText(),
                });
            }
        }
        var when = DateTimeOffset.FromUnixTimeMilliseconds(ts).UtcDateTime;
        if (input.Timestamp == "") input.Timestamp = when.ToString("o", CultureInfo.InvariantCulture);
        if (input.DayOfWeek == "") input.DayOfWeek = when.DayOfWeek.ToString();
        if (float.IsNaN(input.HourOfDay)) input.HourOfDay = when.Hour;
        return input;
    }

    private async Task RegisterOnce(string outSensor, string source, CancellationToken stop)
    {
        if (string.IsNullOrEmpty(_o.HubUrl)) return;
        lock (_registered)
            if (!_registered.Add(outSensor)) return;

        // Probabilities move in jumps by nature, so spike detection is off;
        // only the risk threshold raises anomalies.
        var fields = new Dictionary<string, object>
        {
            ["prediction"] = new { label = "Predicted class" },
            ["confidence"] = new { min = 0, max = 1, detect = new { z = -1 } },
            ["risk"] = new { label = $"1 - P({_o.NormalLabel})", min = 0, max = 1, detect = new { high = _o.RiskThreshold, z = -1 } },
        };
        foreach (var l in _labels)
            fields["p_" + SafeField(l)] = new { label = $"P({l})", min = 0, max = 1, detect = new { z = -1 } };
        var def = new { name = $"{source} · ML.NET", kind = "ml-prediction", fields };

        try
        {
            var http = httpFactory.CreateClient();
            using var req = new HttpRequestMessage(HttpMethod.Put, $"{_o.HubUrl!.TrimEnd('/')}/api/sensors/{outSensor}")
            {
                Content = JsonContent.Create(def),
            };
            if (!string.IsNullOrEmpty(_o.Token))
                req.Headers.Authorization = new AuthenticationHeaderValue("Bearer", _o.Token);
            using var res = await http.SendAsync(req, stop);
            if (!res.IsSuccessStatusCode)
                log.LogWarning("Registering {Sensor} with the hub failed: {Status}", outSensor, res.StatusCode);
        }
        catch (Exception ex) when (!stop.IsCancellationRequested)
        {
            log.LogWarning("Registering {Sensor} with the hub failed: {Error}", outSensor, ex.Message);
            lock (_registered) _registered.Remove(outSensor); // retry on the next event
        }
    }

    // Hub field names allow [A-Za-z0-9_.-].
    internal static string SafeField(string s) => Unsafe().Replace(s, "_");

    [GeneratedRegex("[^A-Za-z0-9_.-]")]
    private static partial Regex Unsafe();
}
