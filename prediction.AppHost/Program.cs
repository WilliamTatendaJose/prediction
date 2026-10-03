

var builder = DistributedApplication.CreateBuilder(args);

// IoT hub (Go): MQTT broker on 1883, API + dashboard on 8080. Needs Go on PATH.
var iotHub = builder.AddExecutable("iot-hub", "go", "../iot-hub", "run", "./cmd/iothub")
    .WithEnvironment("IOTHUB_HTTP", ":8080")
    .WithHttpEndpoint(port: 8080, isProxied: false);

// prediction-server scores access events from the hub (Services/IotHubBridge.cs).
builder.AddProject<Projects.prediction_Server>("prediction-server")
    .WithEnvironment("IotHub__HubUrl", iotHub.GetEndpoint("http"))
    .WaitFor(iotHub);

builder.AddProject<Projects.AccessControlEmulator>("acess-emulator")
    .WithEnvironment("IOTHUB_URL", iotHub.GetEndpoint("http"))
    .WaitFor(iotHub);
builder.AddProject<Projects.prediction_client>("Client");

builder.Build().Run();
